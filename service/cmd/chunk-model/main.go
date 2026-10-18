// Command chunk-model pins a model file's 64MB chunks into its manifest.
//
// Usage: go run ./cmd/chunk-model <model.gguf> <manifest.yaml>
//
// It hashes every 64MB chunk and writes the chunk list into the manifest's
// `chunks` section, preserving all other sections and comments. The manifest
// is the trust root for P2P transfer: receivers verify each chunk's SHA-256
// before assembly, then the whole file's SHA-256.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	jobprotocol "distributed-ai/protocol"
	"distributed-ai/service/internal/model"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <model.gguf> <manifest.yaml>\n", os.Args[0])
		os.Exit(1)
	}
	modelPath, manifestPath := os.Args[1], os.Args[2]

	f, err := os.Open(modelPath)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		fatal(err)
	}

	var chunks []model.ChunkInfo
	buf := make([]byte, jobprotocol.ChunkSize)
	index := 0
	for {
		n, err := io.ReadFull(f, buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if n == 0 {
				break
			}
		} else if err != nil {
			fatal(err)
		}
		sum := sha256.Sum256(buf[:n])
		chunks = append(chunks, model.ChunkInfo{
			Index:     index,
			SizeBytes: int64(n),
			SHA256:    hex.EncodeToString(sum[:]),
		})
		index++
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}
	fmt.Printf("file %s: %d bytes -> %d chunks\n", modelPath, fi.Size(), len(chunks))

	// Splice the chunks section into the manifest, preserving everything else.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		fatal(err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		fatal(fmt.Errorf("manifest %s: expected a YAML mapping", manifestPath))
	}
	mapping := doc.Content[0]

	var chunksNode yaml.Node
	if err := chunksNode.Encode(model.ChunkList{SizeBytes: jobprotocol.ChunkSize, List: chunks}); err != nil {
		fatal(err)
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "chunks"}

	// Replace existing chunks section if present, else append.
	replaced := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "chunks" {
			mapping.Content[i+1] = &chunksNode
			replaced = true
			break
		}
	}
	if !replaced {
		mapping.Content = append(mapping.Content, keyNode, &chunksNode)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(manifestPath, out, 0644); err != nil {
		fatal(err)
	}
	fmt.Printf("manifest %s: chunks section %s (%d chunks)\n",
		manifestPath, map[bool]string{true: "replaced", false: "added"}[replaced], len(chunks))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chunk-model:", err)
	os.Exit(1)
}
