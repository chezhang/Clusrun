package main

import (
	pb "../protobuf"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	grpc "google.golang.org/grpc"
)

var (
	// TODO: use a sync.Map from node to id and 2 arrays instead, only lock when appending
	reportedTime   sync.Map
	validateNumber sync.Map
)

type jobOnNode struct {
	state    pb.JobState
	exitCode int32
}

type headnode_server struct {
	pb.UnimplementedHeadnodeServer
}

func (s *headnode_server) Heartbeat(ctx context.Context, in *pb.HeartbeatRequest) (*pb.Empty, error) {
	defer LogPanicBeforeExit()
	nodename, host := in.GetNodename(), in.GetHost()
	if strings.ContainsAny(nodename, "()") { // TODO: support nodename containing "(" or ")" by using a map of host -> display name, display name -> host should also be considered when parsing node from request input
		LogError("Invalid nodename in heartbeat: %v", nodename)
		return &pb.Empty{}, errors.New("Invalid nodename: " + nodename)
	}
	hostname, port, host, err := ParseHostAddress(host)
	if err != nil {
		LogError("Invalid host format in heartbeat: %v", host)
		return &pb.Empty{}, errors.New("Invalid host format: " + host)
	}
	nodename = strings.ToUpper(nodename)
	var display_name string
	if hostname == nodename && port == DefaultPort {
		display_name = nodename
	} else {
		display_name = nodename + "(" + host + ")"
	}
	if last_report, ok := reportedTime.Load(display_name); !ok {
		LogInfo("First heartbeat from %v", display_name)
	} else if HeartbeatTimeout(last_report.(time.Time)) {
		LogInfo("%v reconnected. Last report time: %v", display_name, last_report)
		validateNumber.Delete(display_name)
	}
	reportedTime.Store(display_name, time.Now())
	go Validate(display_name, nodename, host)
	return &pb.Empty{}, nil
}

func (s *headnode_server) GetNodes(ctx context.Context, in *pb.GetNodesRequest) (*pb.GetNodesReply, error) {
	defer LogPanicBeforeExit()
	pattern, state, _ := in.GetPattern(), in.GetState(), in.GetGroups()
	nodes := []*pb.GetNodesReply_Node{}
	reportedTime.Range(func(key interface{}, val interface{}) bool {
		nodename := key.(string)
		if matched, _ := regexp.MatchString(pattern, nodename); !matched {
			return true
		}
		last_report := val.(time.Time)
		node := pb.GetNodesReply_Node{Name: nodename}
		if HeartbeatTimeout(last_report) {
			node.State = pb.NodeState_Lost
		} else {
			if number, ok := validateNumber.Load(nodename); ok && number.(int) < 0 {
				node.State = pb.NodeState_Ready
			} else {
				node.State = pb.NodeState_Error
			}
		}
		if state == pb.NodeState_Unknown || state == node.State {
			nodes = append(nodes, &node)
		}
		return true
	})
	LogInfo("GetNodes result: %v", nodes)
	return &pb.GetNodesReply{Nodes: nodes}, nil
}

func (s *headnode_server) GetJobs(ctx context.Context, in *pb.GetJobsRequest) (*pb.GetJobsReply, error) {
	defer LogPanicBeforeExit()
	job_ids := in.GetJobIds()
	loaded_jobs, err := LoadJobs()
	if err != nil {
		return nil, err
	}
	job_ids = NormalizeJobIds(job_ids, loaded_jobs)
	get_all := false
	if _, ok := job_ids[JobId_All]; ok {
		get_all = true
	}
	jobs := []*pb.Job{}
	for i := range loaded_jobs {
		if _, ok := job_ids[loaded_jobs[i].Id]; ok || get_all {
			jobs = append(jobs, &loaded_jobs[i])
		}
	}
	LogInfo("GetJobs result:\n%v", jobs)
	return &pb.GetJobsReply{Jobs: jobs}, nil
}

func (s *headnode_server) StartClusJob(in *pb.StartClusJobRequest, out pb.Headnode_StartClusJobServer) error {
	defer LogPanicBeforeExit()
	command, nodes, pattern, sweep := in.GetCommand(), in.GetNodes(), in.GetPattern(), in.GetSweep()
	LogInfo("Creating new job with command: %v", command)

	// Get nodes
	nodes, invalid_nodes := GetValidNodes(nodes, pattern)
	sort.Strings(nodes)
	sort.Strings(invalid_nodes)
	if len(invalid_nodes) > 0 {
		LogWarning("Invalid nodes to create job: %v", invalid_nodes)
		return errors.New(fmt.Sprintf("Invalid nodes (%v): %v", len(invalid_nodes), invalid_nodes))
	}
	if len(nodes) == 0 {
		message := "No valid nodes to create job"
		LogWarning(message)
		return errors.New(message)
	}

	// Parse sweep
	placeholder, sweepSequence := parseSweep(sweep, len(nodes))
	if !strings.Contains(command, placeholder) {
		msg := fmt.Sprintf("Sweep placeholder %v has wrong format or is not in command: %v", placeholder, command)
		LogWarning(msg)
		return errors.New(msg)
	}

	// Create job
	id, err := CreateNewJob(command, sweep, nodes)
	if err != nil {
		LogError("Failed to create job: %v", err)
		return err
	}
	if err := out.Send(&pb.StartClusJobReply{JobId: int32(id), Nodes: nodes}); err != nil {
		LogError("Failed to send job id of job %v to client: %v", id, err)
		return err
	}

	// Start job on nodes in the cluster
	UpdateJobState(id, pb.JobState_Created, pb.JobState_Dispatching)
	wg := sync.WaitGroup{}
	var job_on_nodes sync.Map
	for i, node := range nodes {
		wg.Add(1)
		c := command
		if len(sweep) > 0 {
			c = strings.ReplaceAll(command, placeholder, strconv.Itoa(sweepSequence[i]))
		}
		go StartJobOnNode(id, c, node, &job_on_nodes, out, &wg, Config_Headnode_StoreOutput.GetBool())
	}
	UpdateJobState(id, pb.JobState_Dispatching, pb.JobState_Running)
	wg.Wait()

	// Update job in DB
	failedNodes := map[string]int32{}
	job_on_nodes.Range(func(key interface{}, val interface{}) bool {
		nodename := key.(string)
		j := val.(jobOnNode)
		if j.state == pb.JobState_Failed {
			failedNodes[nodename] = j.exitCode
		}
		return true
	})
	if len(failedNodes) > 0 {
		UpdateFailedJob(id, failedNodes)
	} else {
		UpdateFinishedJob(id)
	}
	return nil
}

func (s *headnode_server) CancelClusJobs(ctx context.Context, in *pb.CancelClusJobsRequest) (*pb.CancelClusJobsReply, error) {
	defer LogPanicBeforeExit()
	job_ids := in.GetJobIds()
	result, to_cancel, err := CancelJobs(job_ids)
	if err != nil {
		LogError("Failed to cancel jobs: %v", err)
		return nil, err
	}
	for id, nodes := range to_cancel {
		go CancelJob(id, nodes)
	}
	LogInfo("CancelClusJobs result: %v", result)
	return &pb.CancelClusJobsReply{Result: result}, nil
}

func (s *headnode_server) SetConfigs(ctx context.Context, in *pb.SetConfigsRequest) (*pb.SetConfigsReply, error) {
	defer LogPanicBeforeExit()
	configs := in.GetConfigs()
	results := SetNodeConfigs(Config_Headnode, configs)
	return &pb.SetConfigsReply{Results: results}, nil
}

func (s *headnode_server) GetConfigs(ctx context.Context, in *pb.Empty) (*pb.GetConfigsReply, error) {
	defer LogPanicBeforeExit()
	results := GetNodeConfigs(Config_Headnode)
	return &pb.GetConfigsReply{Configs: results}, nil
}

func Validate(display_name, nodename, host string) {
	if number, ok := validateNumber.LoadOrStore(display_name, 0); !ok || number.(int) > 0 {
		number := number.(int)
		if ok { // validate immediately in the first time, otherwise double validating interval after every failure
			validateNumber.Store(display_name, 0) // value 0 means validation is ongoing
			delay := math.Pow(2, float64(number))
			if delay > 60 {
				delay = 60
			}
			time.Sleep(time.Duration(delay) * time.Second)
		}
		LogInfo("Start validating clusnode %v", display_name)
		conn, err := grpc.Dial(host, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			LogError("Can not connect: %v", err)
			validateNumber.Store(display_name, number+1)
			return
		}
		defer conn.Close()

		c := pb.NewClusnodeClient(conn)
		LogInfo("Connected to clusnode host %v", host)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		reply, err := c.Validate(ctx, &pb.ValidateRequest{Headnode: NodeHost, Clusnode: host})
		name := strings.ToUpper(reply.GetNodename())
		if err != nil {
			LogError("Validation failed: %v", err)
			validateNumber.Store(display_name, number+1)
		} else if name != nodename { // in case a clusnode is started with a wrong but reachable host
			LogError("Validation failed: expect nodename %v, replied nodename %v", nodename, name)
			validateNumber.Store(display_name, 10)
		} else {
			LogInfo("Clusnode %v is validated that being hosted by %v", display_name, host)
			validateNumber.Store(display_name, -1)
		}
	}
}

func GetValidNodes(nodes []string, pattern string) ([]string, []string) {
	ready_nodes := map[string]string{}
	valid_nodes := []string{}
	reportedTime.Range(func(key interface{}, val interface{}) bool {
		node := key.(string)
		last_report := val.(time.Time)
		if number, ok := validateNumber.Load(node); ok && number.(int) < 0 && !HeartbeatTimeout(last_report) {
			if matched, _ := regexp.MatchString(pattern, node); !matched {
				return true
			}
			ready_nodes[node] = node
			ready_nodes[ParseHost(node)] = node
			valid_nodes = append(valid_nodes, node)
		}
		return true
	})
	invalid_nodes := []string{}
	if len(nodes) > 0 {
		valid_nodes = []string{}
		added := map[string]bool{}
		for _, node := range nodes {
			if valid_node, ok := ready_nodes[strings.ToUpper(node)]; ok {
				if _, ok := added[valid_node]; !ok {
					valid_nodes = append(valid_nodes, valid_node)
					added[valid_node] = true
				}
			} else {
				invalid_nodes = append(invalid_nodes, node)
			}
		}
	}
	return valid_nodes, invalid_nodes
}

func ParseHost(display_name string) string {
	segs := strings.Split(display_name, "(")
	if len(segs) <= 1 {
		return display_name + ":" + DefaultPort
	} else {
		return segs[1][:len(segs[1])-1]
	}
}

func StartJobOnNode(id int, command, node string, job_on_nodes *sync.Map, out pb.Headnode_StartClusJobServer, wg *sync.WaitGroup, save_output bool) {
	defer wg.Done()
	LogInfo("Start job %v on node %v", id, node)

	var f_out, f_err *os.File
	if save_output {
		// Create file to save output
		stdout, stderr := GetOutputFile(id, node)
		var err error
		if f_out, err = os.Create(stdout); err == nil {
			f_err, err = os.Create(stderr)
		}
		if err != nil {
			LogError("Failed to create output file for job %v node %v: %v", id, node, err)
			return
		}
		defer f_out.Close()
		defer f_err.Close()
	}
	job_on_nodes.Store(node, jobOnNode{state: pb.JobState_Dispatching})

	// Setup connection
	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	conn, err := grpc.DialContext(ctx, ParseHost(node), grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		LogError("Can not connect node %v in %v: %v", node, ConnectTimeout, err)
		return
	}
	defer conn.Close()
	c := pb.NewClusnodeClient(conn)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	// Start job on clusnode
	stream, err := c.StartJob(ctx, &pb.StartJobRequest{JobId: int32(id), Command: command, Headnode: NodeHost})
	if err != nil {
		LogError("Failed to start job %v on node %v: %v", id, node, err)
		job_on_nodes.Store(node, jobOnNode{state: pb.JobState_Failed})
		return
	} else {
		job_on_nodes.Store(node, jobOnNode{state: pb.JobState_Running})
	}

	// Save and redirect output
	var exit_code int32 = -1
	failing_to_redirect := false
	for {
		output, err := stream.Recv()
		if err == io.EOF {
			LogInfo("Job %v on node %v finished with exit code %v", id, node, exit_code)
			if err := out.Send(&pb.StartClusJobReply{Node: node, ExitCode: exit_code}); err != nil {
				LogWarning("Failed to redirect exit code of job %v on node %v: %v", id, node, err)
			}
			break
		}
		if err != nil {
			LogError("Failed to receive output of job %v on node %v: %v", id, node, err)
			return
		} else {
			stdout, stderr := output.GetStdout(), output.GetStderr()
			if stdout != "" {
				if save_output {
					if _, err := f_out.WriteString(stdout); err != nil {
						LogError("Failed to save stdout of job %v on node %v: %v", id, node, err)
					}
				}
				if err := out.Send(&pb.StartClusJobReply{Node: node, Stdout: stdout}); err != nil {
					if !failing_to_redirect {
						LogWarning("Failed to redirect stdout of job %v on node %v: %v", id, node, err)
					}
					failing_to_redirect = true
				} else {
					failing_to_redirect = false
				}
			}
			if stderr != "" {
				if save_output {
					if _, err := f_err.WriteString(stderr); err != nil {
						LogError("Failed to save stderr of job %v on node %v: %v", id, node, err)
					}
				}
				if err := out.Send(&pb.StartClusJobReply{Node: node, Stderr: stderr}); err != nil {
					if !failing_to_redirect {
						LogWarning("Failed to redirect stderr of job %v on node %v: %v", id, node, err)
					}
					failing_to_redirect = true
				} else {
					failing_to_redirect = false
				}
			}
			exit_code = output.GetExitCode()
		}
	}
	if exit_code == 0 {
		job_on_nodes.Store(node, jobOnNode{state: pb.JobState_Finished})
	} else {
		job_on_nodes.Store(node, jobOnNode{state: pb.JobState_Failed, exitCode: exit_code})
	}
}

func CancelJob(id int32, nodes []string) {
	wg := sync.WaitGroup{}
	result := sync.Map{}
	for i := range nodes {
		wg.Add(1)
		result.Store(nodes[i], false)
		go CancelJobOnNode(id, nodes[i], &wg, &result)
	}
	wg.Wait()
	var cancel_failed_nodes []string
	result.Range(func(node interface{}, canceled interface{}) bool {
		if !canceled.(bool) {
			cancel_failed_nodes = append(cancel_failed_nodes, node.(string))
		}
		return true
	})
	UpdateCancelledJob(id, cancel_failed_nodes)
}

func CancelJobOnNode(id int32, node string, wg *sync.WaitGroup, result *sync.Map) {
	defer wg.Done()

	// Setup connection
	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	conn, err := grpc.DialContext(ctx, ParseHost(node), grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		LogError("Can not connect node %v in %v: %v", node, ConnectTimeout, err)
		return
	}
	defer conn.Close()
	c := pb.NewClusnodeClient(conn)
	ctx, cancel = context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()

	// Cancel job on clusnode
	_, err = c.CancelJob(ctx, &pb.CancelJobRequest{JobId: id, Headnode: NodeHost})
	if err != nil {
		LogError("Failed to cancel job %v on node %v: %v", id, node, err)
	} else {
		result.Store(node, true)
	}
}

func HeartbeatTimeout(last_report time.Time) bool {
	return time.Since(last_report) > time.Duration(Config_Headnode_HeartbeatTimeoutSecond.GetInt())*time.Second
}

// Valid format: placeholder[{[-]begin[-[-]end][,[-]step]}]
func parseSweep(sweep string, count int) (placeholder string, sequence []int) {
	placeholder = sweep
	sequence = make([]int, count)
	for i := range sequence {
		sequence[i] = i
	}
	begin, end, step := 0, Max_Int, 0
	if length := len(sweep); length == 0 {
		return
	} else if sweep[length-1:] != "}" {
		return
	}
	if index := strings.LastIndex(sweep, "{"); index < 0 {
		return
	} else if len(sweep[0:index]) == 0 {
		return
	} else if parts := strings.Split(sweep[index+1:len(sweep)-1], ","); len(parts) > 2 {
		return
	} else {
		if len(parts) == 2 {
			// Format: placeholder{begin[-end],step}
			if s, err := strconv.Atoi(parts[1]); err != nil {
				return
			} else if s == 0 {
				return
			} else {
				step = s
				if step < 0 {
					end = Min_Int
				}
			}
		}
		parts := strings.Split(parts[0], "-")
		if len(parts) == 1 {
			// Format: placeholder{begin[,step]}
			if b, err := strconv.Atoi(parts[0]); err != nil {
				return
			} else {
				begin = b
			}
		} else if len(parts) == 2 {
			if len(parts[0]) == 0 {
				// Format: placeholder{-begin[,step]}
				if b, err := strconv.Atoi("-" + parts[1]); err != nil {
					return
				} else {
					begin = b
				}
			} else {
				// Format: placeholder{begin-end[,step]}
				if b, err := strconv.Atoi(parts[0]); err != nil {
					return
				} else {
					begin = b
				}
				if e, err := strconv.Atoi(parts[1]); err != nil {
					return
				} else {
					end = e
				}
			}
		} else if len(parts) == 3 {
			if len(parts[0]) == 0 {
				// Format: placeholder{-begin-end[,step]}
				if b, err := strconv.Atoi("-" + parts[1]); err != nil {
					return
				} else {
					begin = b
				}
				if e, err := strconv.Atoi(parts[2]); err != nil {
					return
				} else {
					end = e
				}
			} else if len(parts[1]) == 0 {
				// Format: placeholder{begin--end[,step]}
				if b, err := strconv.Atoi(parts[0]); err != nil {
					return
				} else {
					begin = b
				}
				if e, err := strconv.Atoi("-" + parts[2]); err != nil {
					return
				} else {
					end = e
				}
			} else {
				return
			}
		} else if len(parts) == 4 {
			// Format: placeholder{-begin--end[,step]}
			if len(parts[0]) == 0 || len(parts[2]) == 0 {
				if b, err := strconv.Atoi("-" + parts[1]); err != nil {
					return
				} else {
					begin = b
				}
				if e, err := strconv.Atoi("-" + parts[3]); err != nil {
					return
				} else {
					end = e
				}
			} else {
				return
			}
		} else {
			return
		}
		placeholder = sweep[0:index]
	}
	if step == 0 {
		if begin < end {
			step = 1
		}
		if begin > end {
			step = -1
		}
	}
	n := begin
	for i := range sequence {
		sequence[i] = n
		n += step
		if step > 0 && n > end || step < 0 && n < end {
			n = begin
		}
	}
	return
}
