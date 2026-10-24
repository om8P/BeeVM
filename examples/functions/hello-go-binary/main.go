// hello-go-binary is the example "static binary" function.
//
// Contract: the request payload arrives as JSON on stdin, the result goes to stdout as JSON,
// and anything on stderr becomes the function's log.
//
//	in:  {"name":"Om"}
//	out: {"greeting":"Hello, Om!","runtime":"binary"}
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintln(os.Stderr, "bad input:", err)
		os.Exit(1)
	}
	if in.Name == "" {
		in.Name = "world"
	}
	fmt.Fprintln(os.Stderr, "hello-go-binary: greeting", in.Name)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
		"greeting": fmt.Sprintf("Hello, %s!", in.Name),
		"runtime":  "binary",
	})
}
