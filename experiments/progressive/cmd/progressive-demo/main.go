// Command progressive-demo runs the offline certified-progressive
// experiment and prints the JSON report.
//
// Usage:
//
//	progressive-demo                       # synthetic weights (labeled as such)
//	progressive-demo -gguf model.gguf -tensor output.weight
//	                                       # real weights from GGUF; activations
//	                                       # synthetic unless -activations
//	                                       # points at a hidden-collector dataset
//	                                       # (real hidden states, kind-labeled)
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
	fullVocab := flag.Bool("full-vocab", false, "use ALL weight rows (full vocabulary for lm_head tensors); overrides -max-classes")
	actPath := flag.String("activations", "", "activation dataset JSON (from hidden-collector); replaces synthetic activations")
	allowPooled := flag.Bool("allow-pooled", false, "accept a pooled activation dataset (labeled as such; not exact hidden states)")
	seed := flag.Int64("seed", 42, "seed for synthetic data")
	listOnly := flag.Bool("list", false, "list GGUF tensors and exit")
	flag.Parse()
	if *fullVocab {
		*maxClasses = 0
	}

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
		classNote := fmt.Sprintf("%d of %d rows", len(W), ti.Dims[1])
		if len(W) == int(ti.Dims[1]) {
			classNote = fmt.Sprintf("all %d rows (full vocabulary)", len(W))
		}
		dataSrc := fmt.Sprintf("real GGUF tensor %q (%s) from %s — %s", *tensor, ti.TypeName, *ggufPath, classNote)
		if *actPath != "" {
			set, lerr := progressive.LoadActivations(*actPath)
			if lerr != nil {
				fmt.Fprintln(os.Stderr, "activations:", lerr)
				os.Exit(1)
			}
			if set.Kind == progressive.KindPooled && !*allowPooled {
				fmt.Fprintln(os.Stderr, "activations dataset is pooled_multi_token — not exact hidden states; pass -allow-pooled to run it labeled as pooled")
				os.Exit(2)
			}
			if set.HiddenSize != len(W[0]) {
				fmt.Fprintf(os.Stderr, "activation width %d != weight row width %d\n", set.HiddenSize, len(W[0]))
				os.Exit(1)
			}
			rep, err = progressive.RunExperiment(W, set.Vectors, dataSrc,
				fmt.Sprintf("%s — %s", set.Kind, set.Source))
		} else {
			acts := progressive.SyntheticActivations(*seed, *samples, len(W[0]))
			rep, err = progressive.RunExperiment(W, acts, dataSrc,
				"synthetic Gaussian activations (seeded)")
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "experiment:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}
