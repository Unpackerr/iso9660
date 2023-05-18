package main

import (
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/kdomanski/iso9660"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: %s <filename.iso>", os.Args[0])
	}

	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatalf("failed to open %s: %s", os.Args[1], err)
	}
	defer f.Close()

	iso, err := iso9660.OpenImage(f)
	if err != nil {
		log.Fatalf("failed to open image %s: %s", os.Args[1], err)
	}

	root, err := iso.RootDir()
	if err != nil {
		log.Fatalf("failed to open iso root %s: %s", os.Args[1], err)
	}

	if err := printEntries(root, []string{}); err != nil {
		log.Fatal(err)
	}
}

func printEntries(file *iso9660.File, parents []string) error {
	if len(parents) > 1 && (file.Name() == "\x00" || file.Name() == "\x01") {
		return nil
	}

	printEntry(file, parents)

	if !file.IsDir() {
		return nil
	}

	childs, err := file.GetChildren()
	if err != nil {
		return fmt.Errorf("getting children for %s: %w", file.Name(), err)
	}

	for _, entry := range childs {
		if err := printEntries(entry, append(parents, file.Name())); err != nil {
			return err
		}
	}

	return nil
}

func printEntry(file *iso9660.File, parents []string) {
	path := path.Join(append(parents, file.Name())...)
	path = strings.ReplaceAll(path, "\x00", ".")
	path = strings.ReplaceAll(path, "\x01", "..")

	fmt.Printf("%q age: %v bytes: %v, me: %v\n",
		path,
		time.Since(file.ModTime()).Round(time.Second),
		file.Size(), file.HasMultiExtent())
}
