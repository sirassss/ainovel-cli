package eval

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
)

// Command is the entry point of the `ainovel-cli eval` subcommand and returns the process exit code:
// 0=PASS/WARN, 1=some case FAILed, 2=usage/config error.
//
// Flow: load config -> load cases -> run per single/A-B orchestration -> collect -> grade ->
// aggregate -> report.
func Command(argv []string) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	casesPath := fs.String("cases", "", "case directory or a single .json file (required)")
	variantDir := fs.String("variant", "", "variant prompt override directory (containing core prompts such as writer.md)")
	configPath := fs.String("config", "", "config file path (default path if omitted)")
	outDir := fs.String("out", "", "report output directory (default workspace/evals/<run_id>)")
	maxChapters := fs.Int("max-chapters", -1, "override the chapter cap of every case (-1 = no override)")
	timeout := fs.Duration("timeout", 30*time.Minute, "wall-clock limit per case (0 = unlimited)")
	repeat := fs.Int("repeat", 1, "number of times to repeat each case (reduces the effect of model randomness)")
	ci := fs.Bool("ci", false, "CI mode: suppress per-event progress output and print only the final verdict (the exit code already reflects the gate, with or without this flag)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if strings.TrimSpace(*casesPath) == "" {
		fmt.Fprintln(os.Stderr, "eval: missing --cases")
		fs.Usage()
		return 2
	}
	if *repeat <= 0 {
		fmt.Fprintln(os.Stderr, "eval: --repeat must be greater than 0")
		return 2
	}

	// When eval's -config points at a standalone file it is loaded as a single file (reproducible,
	// not polluted by the machine's global/project config); otherwise the default two-layer
	// global+project merge applies.
	loadConfig := bootstrap.LoadConfig
	if strings.TrimSpace(*configPath) != "" {
		loadConfig = func() (bootstrap.Config, error) { return bootstrap.LoadConfigFile(*configPath) }
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval: load config failed: %v\n", err)
		return 2
	}

	cases, err := LoadCases(*casesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval: load cases failed: %v\n", err)
		return 2
	}

	variantPrompts, err := loadVariant(*variantDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval: load variant failed: %v\n", err)
		return 2
	}

	runID := time.Now().Format("20060102-150405")
	if *outDir == "" {
		*outDir = filepath.Join("workspace", "evals", runID)
	}
	variantName := ""
	if *variantDir != "" {
		variantName = filepath.Base(*variantDir)
	}

	mode := "single"
	if variantName != "" {
		mode = "ab"
	}
	fmt.Fprintf(os.Stderr, "eval run %s · %d cases · mode=%s · variant=%s · repeat=%d\n",
		runID, len(cases), mode, orNone(variantName), *repeat)

	caseResults := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		if *maxChapters >= 0 {
			c.MaxChapters = *maxChapters
		}
		fmt.Fprintf(os.Stderr, "\n▶ %s (%s)\n", c.ID, c.Category)

		style := c.Style
		if style == "" {
			style = cfg.Style
		}
		var progressW io.Writer
		if !*ci {
			progressW = os.Stderr // CI mode silences per-event output to keep the log clean
		}

		if variantName == "" {
			runs := make([]RunResult, 0, *repeat)
			for i := 1; i <= *repeat; i++ {
				bundle := assets.Load(style, assets.LoadOptions{}) // built-in only: a deterministic baseline, unaffected by local overrides
				dir := runDir(*outDir, c.ID, ArmSingle, i, *repeat)
				res := runOne(cfg, bundle, c, dir, *timeout, progressW)
				res.Arm, res.Repeat = ArmSingle, i
				runs = append(runs, RunResult{Arm: ArmSingle, Repeat: i, Result: res})
				fmt.Fprintf(os.Stderr, "  → single#%d %s\n", i, res.Outcome)
			}
			caseResults = append(caseResults, NewSingleRunsCaseResult(c, runs))
			continue
		}

		runs := make([]RunResult, 0, *repeat*2)
		deltas := make([]Delta, 0, *repeat)
		for i := 1; i <= *repeat; i++ {
			baseBundle := assets.Load(style, assets.LoadOptions{})
			baseDir := runDir(*outDir, c.ID, ArmBaseline, i, *repeat)
			base := runOne(cfg, baseBundle, c, baseDir, *timeout, progressW)
			base.Arm, base.Repeat = ArmBaseline, i
			runs = append(runs, RunResult{Arm: ArmBaseline, Repeat: i, Result: base})
			fmt.Fprintf(os.Stderr, "  → baseline#%d %s\n", i, base.Outcome)

			varBundle := assets.Load(style, assets.LoadOptions{})
			if err := applyVariant(&varBundle, variantPrompts); err != nil {
				fmt.Fprintf(os.Stderr, "eval: apply variant override failed: %v\n", err)
				return 2
			}
			varDir := runDir(*outDir, c.ID, ArmVariant, i, *repeat)
			variant := runOne(cfg, varBundle, c, varDir, *timeout, progressW)
			variant.Arm, variant.Repeat = ArmVariant, i
			runs = append(runs, RunResult{Arm: ArmVariant, Repeat: i, Result: variant})
			delta := GradeDelta(c, base, variant)
			deltas = append(deltas, delta)
			fmt.Fprintf(os.Stderr, "  → variant#%d %s · delta %s\n", i, variant.Outcome, delta.Outcome)
		}
		caseResults = append(caseResults, NewABCaseResult(c, runs, deltas))
	}

	suite := Aggregate(runID, mode, variantName, *repeat, caseResults)
	if err := WriteReport(suite, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "eval: write report failed: %v\n", err)
		return 2
	}

	fmt.Fprintf(os.Stderr, "\n%s\nReport: %s\n", Summary(suite), filepath.Join(*outDir, "report.md"))
	if suite.Gate == Fail {
		return 1
	}
	return 0
}

func runOne(cfg bootstrap.Config, bundle assets.Bundle, c Case, dir string, timeout time.Duration, progressW io.Writer) Result {
	runErr := RunCase(cfg, bundle, c, RunOptions{
		OutputDir: dir,
		Timeout:   timeout,
		Progress:  progressW,
	})
	col := Collect(dir, runErr)
	return Grade(c, col)
}

func runDir(outDir, caseID, arm string, repeat, totalRepeats int) string {
	if totalRepeats <= 1 {
		if arm == ArmSingle {
			return filepath.Join(outDir, "artifacts", caseID)
		}
		return filepath.Join(outDir, "artifacts", caseID, arm)
	}
	if arm == ArmSingle {
		return filepath.Join(outDir, "artifacts", caseID, fmt.Sprintf("r%d", repeat))
	}
	return filepath.Join(outDir, "artifacts", caseID, fmt.Sprintf("r%d", repeat), arm)
}

// loadVariant reads every *.md under the variant directory (file name -> content). An empty
// directory returns an empty map.
func loadVariant(dir string) (map[string]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[e.Name()] = string(data)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("variant directory has no *.md file: %s", dir)
	}
	return out, nil
}

func applyVariant(b *assets.Bundle, prompts map[string]string) error {
	for file, raw := range prompts {
		// voice.md is the standalone variant entry for the voice layer: it only replaces the
		// voice section and leaves the protocol templates alone, and assembly still goes through
		// the same BuildWriterPrompt path (docs/voice-layer.md section 3.6).
		if file == "voice.md" {
			b.OverrideVoice(raw)
			continue
		}
		if err := b.OverridePrompt(file, raw); err != nil {
			return err
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}
