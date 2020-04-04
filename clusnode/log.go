package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
)

func LogInfo(format string, v ...interface{}) {
	writeLog(logLevel_Info, format, v...)
}

func LogWarning(format string, v ...interface{}) {
	writeLog(logLevel_Warning, format, v...)
}

func LogError(format string, v ...interface{}) {
	writeLog(logLevel_Error, format, v...)
}

func LogFatality(format string, v ...interface{}) {
	LogError(format, v...)
	fmt.Printf(format, v...)
	os.Exit(1)
}

type logLevel string

const (
	logLevel_Info    = "Info"
	logLevel_Warning = "Warning"
	logLevel_Error   = "Error"
)

func writeLog(level logLevel, format string, v ...interface{}) {
	prefix := fmt.Sprintf("| %v | ", level)
	if Config_LogGoId.GetBool() {
		prefix += fmt.Sprintf("%v | ", currentGoId())
	}
	log.Printf(prefix+format, v...)
}

// Low performance
func currentGoId() string {
	buf := make([]byte, 32)
	runtime.Stack(buf, false)
	s := strings.Fields(string(buf))
	if len(s) > 1 {
		return s[1]
	}
	return "Unknown"
}
