package main

// 文件大小和实际 HTTP 静态模块检查，防止重构后缺文件或模块未发布。

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDemoFilesStaySmall(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".js" && ext != ".mjs" && ext != ".py" && ext != ".html" && ext != ".css" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if lines := strings.Count(string(data), "\n"); lines > 500 {
			t.Errorf("%s 有 %d 行，应拆分职责", path, lines)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAllWebModulesAreServedWithoutCycles(t *testing.T) {
	f := setupDemo(t)
	handler, err := f.st.handler()
	if err != nil {
		t.Fatal(err)
	}
	modules, err := fs.Glob(webFS, "web/*.js")
	if err != nil {
		t.Fatal(err)
	}
	imports := regexp.MustCompile(`from\s+"\./([^"]+)"`)
	graph := map[string][]string{}
	for _, path := range modules {
		name := strings.TrimPrefix(path, "web/")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/"+name, nil))
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
			t.Errorf("模块无法加载: %s %d %v", name, w.Code, w.Header())
		}
		data, _ := fs.ReadFile(webFS, path)
		for _, m := range imports.FindAllStringSubmatch(string(data), -1) {
			graph[name] = append(graph[name], m[1])
		}
	}
	seen, active := map[string]bool{}, map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if active[name] {
			t.Fatalf("模块循环依赖: %s", name)
		}
		if seen[name] {
			return
		}
		seen[name] = true
		active[name] = true
		for _, child := range graph[name] {
			if _, err := fs.Stat(webFS, "web/"+child); err != nil {
				t.Error(err)
			}
			visit(child)
		}
		active[name] = false
	}
	for _, path := range modules {
		visit(strings.TrimPrefix(path, "web/"))
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(w.Body.String(), `type="module"`) {
		t.Fatal("页面未使用模块入口")
	}
}
