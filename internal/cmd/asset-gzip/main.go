package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: asset-gzip INPUT OUTPUT\n")
		os.Exit(2)
	}

	if err := compress(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "asset-gzip: %v\n", err)
		os.Exit(1)
	}
}

func compress(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.CreateTemp(filepath.Dir(destination), ".asset-gzip-*")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer os.Remove(temporary)

	compressed, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		output.Close()
		return err
	}
	compressed.Header.OS = 255

	if _, err := io.Copy(compressed, input); err != nil {
		compressed.Close()
		output.Close()
		return err
	}
	if err := compressed.Close(); err != nil {
		output.Close()
		return err
	}
	if err := output.Chmod(0o644); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}

	return os.Rename(temporary, destination)
}
