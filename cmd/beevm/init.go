package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// Starter files for `beevm init`. Each one is a complete, working function that shows the
// contract: JSON in, JSON out, prints become the log.
var initTemplates = map[string]map[string]string{
	"python": {
		"handler.py": `"""A BeeVM function.

BeeVM imports this file once per VM and calls handler(event) for every request. event is
the call's JSON payload; return anything JSON can hold. print() output is the function's log
(see it with: beevm invoke <name> -l). Libraries you need go into this folder.
"""


def handler(event):
    name = (event or {}).get("name") or "world"
    print("greeting", name)
    return {"greeting": f"Hello, {name}!"}
`,
	},
	"node": {
		"index.js": `// A BeeVM function.
//
// BeeVM loads this file once per VM and calls handler(event) for every request. event is the
// call's JSON payload; return (or resolve) anything JSON can hold. console.log output is the
// function's log (see it with: beevm invoke <name> -l). Modules you need go into this folder.

exports.handler = async (event) => {
  const name = (event && event.name) || 'world';
  console.log('greeting', name);
  return { greeting: 'Hello, ' + name + '!' };
};
`,
	},
	"binary": {
		"main.go": `// A BeeVM function as a static binary.
//
// The call's JSON payload arrives on stdin; write the result as JSON to stdout. Anything on
// stderr is the function's log (see it with: beevm invoke <name> -l). Build it static, for
// Linux x86-64 (the guest is Alpine/musl):
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o function .
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	var in struct {
		Name string ` + "`json:\"name\"`" + `
	}
	_ = json.NewDecoder(os.Stdin).Decode(&in) // no payload: in stays empty
	if in.Name == "" {
		in.Name = "world"
	}
	fmt.Fprintln(os.Stderr, "greeting", in.Name)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"greeting": "Hello, " + in.Name + "!"})
}
`,
		"go.mod": "module {{name}}\n\ngo 1.24\n",
	},
}

var unsafeNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// functionName turns a folder name into a valid function name (lowercase letters, digits,
// '-'; 1-63 characters; see registry.ValidName).
func functionName(dir string) string {
	n := unsafeNameChars.ReplaceAllString(strings.ToLower(filepath.Base(filepath.Clean(dir))), "-")
	n = strings.Trim(n, "-")
	if len(n) > 63 {
		n = strings.Trim(n[:63], "-")
	}
	if n == "" {
		n = "my-function"
	}
	return n
}

// writeTemplate creates dir with the runtime's starter files. It refuses to touch a folder
// that already has files in it.
func writeTemplate(runtime, dir string) ([]string, error) {
	files, ok := initTemplates[runtime]
	if !ok {
		return nil, fmt.Errorf("unknown runtime %q: use python, node or binary", runtime)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s already exists and is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for name, body := range files {
		body = strings.ReplaceAll(body, "{{name}}", functionName(dir))
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return written, err
		}
		written = append(written, p)
	}
	return written, nil
}

func newInit() *cobra.Command {
	return &cobra.Command{
		Use:   "init <python|node|binary> [folder]",
		Short: "Create a folder with a working starter function",
		Long: "Writes a small hello function for the runtime into folder (default hello-<runtime>) and\n" +
			"prints the commands to deploy and call it. binary is a Go program.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			runtime := args[0]
			dir := "hello-" + runtime
			if len(args) == 2 {
				dir = args[1]
			}
			files, err := writeTemplate(runtime, dir)
			if err != nil {
				return err
			}
			for _, f := range files {
				fmt.Println("created", f)
			}
			name := functionName(dir)
			fmt.Println("\nnext:")
			if runtime == "binary" {
				fmt.Printf("  (cd %s && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o function .)\n", dir)
				fmt.Printf("  beevm deploy %s %s\n", name, filepath.Join(dir, "function"))
			} else {
				fmt.Printf("  beevm deploy %s %s\n", name, dir)
			}
			fmt.Printf("  beevm invoke %s --payload '{\"name\":\"you\"}' -l\n", name)
			return nil
		},
	}
}
