// Engine auto-install & backend selection.
//
// The Node Runtime never builds llama.cpp. It detects the platform, picks an
// ordered backend candidate list, downloads the prebuilt engine package for
// the first candidate that passes a self-test, and verifies its hash.
//
// "실제" 원칙: backend 선택은 추측이 아니라 self-test 통과로 결정한다.
package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Platform describes the host for engine selection.
type Platform struct {
	OS        string // linux, windows, darwin
	Arch      string // amd64, arm64
	GPUVendor string // nvidia, amd, intel, apple, unknown
}

// DetectPlatform fills OS/Arch from the runtime and GPU vendor best-effort.
// GPU detection is a hint only; the self-test is the real arbiter.
func DetectPlatform() Platform {
	p := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH, GPUVendor: "unknown"}
	switch {
	case p.OS == "darwin" && p.Arch == "arm64":
		p.GPUVendor = "apple"
	case commandExists("nvidia-smi"):
		p.GPUVendor = "nvidia"
	case fileExists("/dev/kfd"):
		p.GPUVendor = "amd" // ROCm render node
	}
	return p
}

// CandidateBackends returns backends to try, best first.
func CandidateBackends(p Platform) []string {
	switch p.OS {
	case "darwin":
		if p.Arch == "arm64" {
			return []string{"metal", "cpu"}
		}
		return []string{"cpu"}
	case "windows":
		switch p.GPUVendor {
		case "nvidia":
			return []string{"cuda", "vulkan", "cpu"}
		case "amd", "intel":
			return []string{"vulkan", "cpu"}
		}
		return []string{"vulkan", "cpu"}
	default: // linux
		switch p.GPUVendor {
		case "nvidia":
			return []string{"cuda", "vulkan", "cpu"}
		case "amd":
			return []string{"rocm", "vulkan", "cpu"}
		}
		return []string{"vulkan", "cpu"}
	}
}

// SelectBackend downloads candidates in order and returns the first whose
// binary passes self-test (llama-server --version exits 0).
func SelectBackend(ctx context.Context, baseURL string, destDir string) (backend string, binPath string, err error) {
	p := DetectPlatform()
	arch := p.Arch
	if arch == "arm64" && p.OS != "darwin" {
		arch = "arm64"
	}
	for _, b := range CandidateBackends(p) {
		path, err := Install(ctx, baseURL, p.OS, arch, b, destDir)
		if err != nil {
			continue
		}
		if selfTest(ctx, path) {
			return b, path, nil
		}
		_ = os.RemoveAll(filepath.Dir(path))
	}
	return "", "", fmt.Errorf("engine: no working backend found for %s/%s", p.OS, p.Arch)
}

// Install downloads and hash-verifies one engine package, extracts it,
// and returns the llama-server binary path.
func Install(ctx context.Context, baseURL, osName, arch, backend, destDir string) (string, error) {
	name := fmt.Sprintf("engine-%s-%s-%s.tar.gz", osName, arch, backend)
	pkgDir := filepath.Join(destDir, osName+"-"+arch+"-"+backend)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return "", err
	}

	tarURL := strings.TrimSuffix(baseURL, "/") + "/" + name
	wantHex, err := fetchSHA256(ctx, tarURL+".sha256")
	if err != nil {
		return "", fmt.Errorf("engine: fetch sha256: %w", err)
	}

	tmp, err := os.CreateTemp("", "engine-*.tar.gz")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := download(ctx, tarURL, tmp, wantHex); err != nil {
		_ = tmp.Close()
		return "", err
	}
	_ = tmp.Close()

	if err := extractTarGz(tmpName, pkgDir); err != nil {
		return "", fmt.Errorf("engine: extract: %w", err)
	}

	bin := filepath.Join(pkgDir, "llama-server")
	if osName == "windows" {
		bin += ".exe"
	}
	if !fileExists(bin) {
		// fall back: first executable-looking file in pkgDir
		entries, _ := os.ReadDir(pkgDir)
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "llama-server") {
				bin = filepath.Join(pkgDir, e.Name())
				break
			}
		}
	}
	if !fileExists(bin) {
		return "", fmt.Errorf("engine: llama-server not found in package %s", name)
	}
	return bin, nil
}

func fetchSHA256(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	// allow "hex  filename" format
	hexStr := strings.Fields(strings.TrimSpace(string(b)))[0]
	if len(hexStr) != 64 {
		return "", fmt.Errorf("bad sha256 length")
	}
	return hexStr, nil
}

func download(ctx context.Context, url string, w io.Writer, wantHex string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantHex {
		return fmt.Errorf("engine: hash mismatch (tampered or corrupt package)")
	}
	return nil
}

func extractTarGz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("engine: tar path escape: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			_ = out.Close()
		}
	}
}

// selfTest runs the binary with --version; exit 0 means the backend loads
// (missing GPU driver/dll fails here, not at inference time).
//
// This is INSTALL verification only. It says nothing about whether the
// backend can actually compute: a real compute smoke-test (tiny inference)
// is added in M2 when the Node Runtime merges. Do not treat this as
// GPU/backend compute validation.
func selfTest(ctx context.Context, binPath string) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "--version")
	return cmd.Run() == nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
