// matrix is an example "static binary" function: it multiplies two fixed 8x8 matrices.
//
// Contract (same as hello-go-binary): the payload arrives as JSON on stdin (not used here),
// the result goes to stdout as JSON, and anything on stderr becomes the function's log.
//
//	out: {"result":[[0,0,...],...],"size":8}
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// multiply8x8 performs standard 3-loop matrix multiplication on fixed 8x8 arrays
func multiply8x8(a, b [8][8]int) [8][8]int {
	var result [8][8]int

	for i := 0; i < 8; i++ {
		for j := 0; j < 8; j++ {
			for k := 0; k < 8; k++ {
				result[i][j] += a[i][k] * b[k][j]
			}
		}
	}
	return result
}

func main() {
	var matrixA, matrixB [8][8]int

	// Populate sample 8x8 matrices
	for i := 0; i < 8; i++ {
		for j := 0; j < 8; j++ {
			matrixA[i][j] = i + j
			matrixB[i][j] = i * j
		}
	}

	result := multiply8x8(matrixA, matrixB)

	// stdout must be JSON only; human-readable text goes to stderr (the function's log).
	fmt.Fprintln(os.Stderr, "matrix: multiplied two 8x8 matrices")
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"size": 8, "result": result}); err != nil {
		fmt.Fprintln(os.Stderr, "matrix: cannot write result:", err)
		os.Exit(1)
	}
}
