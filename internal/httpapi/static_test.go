package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillMDEndpoint(t *testing.T) {
	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	t.Setenv("SKILL_MD_PATH", "../../skill.md")

	resp, err := http.Get(ts.URL + "/skill.md")
	if err != nil {
		t.Fatalf("get skill.md: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("content-type=%q", resp.Header.Get("Content-Type"))
	}
}

func TestNodeJSLoopMDEndpoint(t *testing.T) {
	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nodejs_loop.md")
	if err != nil {
		t.Fatalf("get nodejs_loop.md: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("content-type=%q", resp.Header.Get("Content-Type"))
	}
}

func TestPythonLoopMDEndpoint(t *testing.T) {
	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/python_loop.md")
	if err != nil {
		t.Fatalf("get python_loop.md: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("content-type=%q", resp.Header.Get("Content-Type"))
	}
}

func TestSkillMDEndpointMethodNotAllowed(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(NewRouter())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/skill.md", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestReadSkillMDFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(target, []byte("# skill"), 0o644); err != nil {
		t.Fatalf("write skill.md: %v", err)
	}

	body, err := readSkillMD(target)
	if err != nil {
		t.Fatalf("readSkillMD: %v", err)
	}
	if string(body) != "# skill" {
		t.Fatalf("unexpected body=%q", string(body))
	}
}
