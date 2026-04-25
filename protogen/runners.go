package protogen

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FakeRunner returns a Runner that, on each invocation, produces the given
// outputs (paths relative to genRoot, content as-is). Useful in tests.
func FakeRunner(outputs map[string]string) Runner {
	return func(ctx context.Context, protoRoot, genRoot string) ([]GeneratedFile, string, error) {
		var files []GeneratedFile
		for rel, content := range outputs {
			files = append(files, GeneratedFile{
				Path:  filepath.Join(genRoot, rel),
				Bytes: []byte(content),
			})
		}
		return files, "fake runner ok", nil
	}
}

// FailingRunner always errors. Useful for self-healing tests.
func FailingRunner(msg string) Runner {
	return func(ctx context.Context, protoRoot, genRoot string) ([]GeneratedFile, string, error) {
		return nil, msg, fmt.Errorf("%s", msg)
	}
}

// ExecRunner returns a Runner that invokes an external command (e.g. "buf
// generate") in the proto root, then walks genRoot to collect the resulting
// files. The command's stdout+stderr is returned as the output string.
//
// For `buf generate`, the typical config is:
//   runner := ExecRunner("buf", []string{"generate"}, genRoot)
// where buf.gen.yaml lives in protoRoot and writes outputs under genRoot.
//
// Note: this Runner reads outputs from disk after the command completes,
// because buf doesn't tell us which files it wrote. The DiffWriter will
// still skip writes for unchanged content, so the cascade is suppressed.
func ExecRunner(bin string, args []string, genRoot string) Runner {
	return func(ctx context.Context, protoRoot, _ string) ([]GeneratedFile, string, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = protoRoot
		out, err := cmd.CombinedOutput()
		output := string(out)
		if err != nil {
			return nil, output, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
		}
		// Walk genRoot collecting all .go files (or whatever the generator emits).
		var files []GeneratedFile
		walkErr := filepath.Walk(genRoot, func(path string, info os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if info.IsDir() {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			files = append(files, GeneratedFile{Path: path, Bytes: data})
			return nil
		})
		if walkErr != nil {
			return nil, output, walkErr
		}
		return files, output, nil
	}
}
