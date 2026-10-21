// Command progressive-demo runs the offline certified-progressive
// experiment and prints the JSON report.
//
// Usage:
//
//	progressive-demo                       # synthetic weights (labeled as such)
//	progressive-demo -gguf model.gguf -tensor output.weight
//	                                       # real weights extracted from GGUF;
//	                                       # activations stay synthetic (no
//	                                       # activation-capture path exists yet)
//
// The report's honesty fields (guarantee scope, refinement status, extra
// RTT) are printed as-is; nothing here claims Transformer-wide guarantees
// or progressive streaming.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"distributed-ai/experiments/progressive"
)

func main() {
	ggufPath := flag.String("gguf", "", "GGUF model file to extract weights from")
	tensor := flag.String("tensor", "", "tensor name to use as the affine layer weights")
	samples := flag.Int("samples", 200, "activation samples")
	maxClasses := flag.Int("max-classes", 128, "cap on weight rows (classes) used")
	seed := flag.Int64("seed", 42, "seed for synthetic data")
	listOnly := flag.Bool("list", false, "list GGUF tensors and exit")
	flag.Parse()

	var rep progressive.Report
	var err error
	if *ggufPath == "" {
		if *tensor != "" {
			fmt.Fprintln(os.Stderr, "-tensor requires -gguf")
			os.Exit(2)
		}
		rep, err = progressive.RunSynthetic(*seed, *samples, 32, 16)
	} else {
		f, ferr := os.Open(*ggufPath)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, ferr)
			os.Exit(1)
		}
		defer f.Close()
		st, _ := f.Stat()
		gf, perr := progressive.ParseGGUF(f, st.Size())
		if perr != nil {
			fmt.Fprintln(os.Stderr, "parse:", perr)
			os.Exit(1)
		}
		if *listOnly {
			for _, t := range gf.Tensors {
				fmt.Printf("%-40s %-6s %v\n", t.Name, t.TypeName, t.Dims)
			}
			return
		}
		if *tensor == "" {
			fmt.Fprintln(os.Stderr, "-tensor is required with -gguf (or use -list)")
			os.Exit(2)
		}
		ti, ok := gf.Find(*tensor)
		if !ok {
			fmt.Fprintf(os.Stderr, "tensor %q not found; %d tensors available (use -list)\n", *tensor, len(gf.Tensors))
			os.Exit(1)
		}
		flat, xerr := gf.ExtractTensor(f, st.Size(), *tensor, 0)
		if xerr != nil {
			fmt.Fprintln(os.Stderr, "extract:", xerr)
			os.Exit(1)
		}
		W, werr := progressive.WeightsFromTensor(flat, ti.Dims, *maxClasses)
		if werr != nil {
			fmt.Fprintln(os.Stderr, "weights:", werr)
			os.Exit(1)
		}
		acts := progressive.SyntheticActivations(*seed, *samples, len(W[0]))
		rep, err = progressive.RunExperiment(W, acts,
			fmt.Sprintf("real GGUF tensor %q (%s) from %s", *tensor, ti.TypeName, *ggufPath),
			"synthetic Gaussian activations (seeded) — no activation capture path exists yet")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "experiment:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}
