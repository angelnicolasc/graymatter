// Command plugin-hello is a reference GrayMatter plugin that implements the
// hello_greet MCP tool.
//
// Protocol:
//
//	stdin:  {"tool":"hello_greet","input":{"name":"Alice"}}\n
//	stdout: {"output":"Hello, Alice!"}\n
//
// Build:
//
//	CGO_ENABLED=0 go build -o plugin-hello ./examples/plugin-hello
//
// Install:
//
//	graymatter plugin install examples/plugin-hello/manifest.json
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type request struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

type response struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		writeError("no input received")
		os.Exit(1)
	}

	var req request
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		writeError(fmt.Sprintf("parse request: %v", err))
		os.Exit(1)
	}

	var resp response
	switch req.Tool {
	case "hello_greet":
		name, _ := req.Input["name"].(string)
		if name == "" {
			name = "world"
		}
		resp.Output = fmt.Sprintf("Hello, %s!", name)

	default:
		resp.Error = fmt.Sprintf("unknown tool %q", req.Tool)
	}

	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
}

func writeError(msg string) {
	data, _ := json.Marshal(response{Error: msg})
	fmt.Println(string(data))
}
