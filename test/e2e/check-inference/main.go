// check-inference validates a llama.cpp completion used by the GPU smoke test.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

type completion struct {
	Content         string `json:"content"`
	TokensPredicted *int   `json:"tokens_predicted"`
	StopType        string `json:"stop_type"`
}

func checkInference(input io.Reader, maxTokens int) error {
	if maxTokens <= 0 {
		return fmt.Errorf("max-tokens must be positive")
	}
	decoder := json.NewDecoder(input)
	var response completion
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("invalid inference response JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("inference response must contain exactly one JSON object")
	}
	if response.TokensPredicted == nil {
		return fmt.Errorf("inference response is missing integer tokens_predicted (stop_type=%q)", response.StopType)
	}
	count := *response.TokensPredicted
	if count <= 0 || count > maxTokens {
		return fmt.Errorf("inference response tokens_predicted=%d (stop_type=%q); expected 1..%d", count, response.StopType, maxTokens)
	}
	if strings.TrimSpace(response.Content) == "" {
		return fmt.Errorf("inference response has empty content (tokens_predicted=%d, stop_type=%q)", count, response.StopType)
	}
	return nil
}

func run(args []string, input io.Reader, stderr io.Writer) error {
	flags := flag.NewFlagSet("check-inference", flag.ContinueOnError)
	flags.SetOutput(stderr)
	maxTokens := flags.Int("max-tokens", 16, "maximum requested completion tokens")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return checkInference(input, *maxTokens)
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "inference check:", err)
		os.Exit(1)
	}
}
