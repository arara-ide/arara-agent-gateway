package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fakeDocker = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "$1 $2" in
  "image inspect")
    if [ -f "$FAKE_DOCKER_STATE/built" ]; then echo sha256:new; elif [ -n "$FAKE_PREVIOUS" ]; then echo "$FAKE_PREVIOUS"; else exit 1; fi ;;
  "build --pull")
    [ -n "$FAKE_BUILD_FAIL" ] && exit 1
    touch "$FAKE_DOCKER_STATE/built" ;;
esac
exit 0
`

func runBuildRuntime(t *testing.T, env ...string) ([]string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "docker.log")
	cmd := exec.Command("bash", "scripts/build-runtime.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ENV_FILE="+filepath.Join(dir, "missing.env"),
		"RUNTIME_IMAGE=aag-runtime:test",
		"FAKE_DOCKER_LOG="+logFile,
		"FAKE_DOCKER_STATE="+dir,
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("output: %s", out)
	}
	data, _ := os.ReadFile(logFile)
	return strings.Split(strings.TrimSpace(string(data)), "\n"), err
}

func indexOf(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

const anchorCreate = "create --name aag-runtime-test-anchor --label aag.role=runtime-anchor --network none --entrypoint true aag-runtime:test"

func TestBuildRuntimeAnchorsNewImageBeforeRemovingPrevious(t *testing.T) {
	calls, err := runBuildRuntime(t, "FAKE_PREVIOUS=sha256:old")
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	remove := indexOf(calls, "rm -f aag-runtime-test-anchor")
	create := indexOf(calls, anchorCreate)
	dropOld := indexOf(calls, "image rm sha256:old")
	if remove < 0 || create < 0 || dropOld < 0 {
		t.Fatalf("missing docker calls: %v", calls)
	}
	if !(remove < create && create < dropOld) {
		t.Fatalf("anchor must be recreated before the previous image is removed: %v", calls)
	}
}

func TestBuildRuntimeAnchorsFirstImage(t *testing.T) {
	calls, err := runBuildRuntime(t)
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	if indexOf(calls, anchorCreate) < 0 {
		t.Fatalf("anchor container not created: %v", calls)
	}
	if indexOf(calls, "image rm") >= 0 {
		t.Fatalf("no previous image should be removed: %v", calls)
	}
}

func TestBuildRuntimeKeepsAnchorWhenBuildFails(t *testing.T) {
	calls, err := runBuildRuntime(t, "FAKE_PREVIOUS=sha256:old", "FAKE_BUILD_FAIL=1")
	if err != nil {
		t.Fatalf("script must keep the current image: %v", err)
	}
	if indexOf(calls, anchorCreate) < 0 {
		t.Fatalf("current image must stay anchored: %v", calls)
	}
	if indexOf(calls, "image rm") >= 0 {
		t.Fatalf("current image must not be removed: %v", calls)
	}
}
