package frontendgen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture 造一个和真实仓库同形状的 web/：站点、多站点、库、非法名、没有 vite.config。
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "web", "admin", "package.json"), `{"name":"admin"}`)
	writeFile(t, filepath.Join(root, "web", "admin", "vite.config.ts"), "export default {}")
	writeFile(t, filepath.Join(root, "web", "memory", "package.json"), `{"name":"memory-site"}`)
	writeFile(t, filepath.Join(root, "web", "memory", "vite.config.js"), "module.exports = {}")
	writeFile(t, filepath.Join(root, "web", "story", "package.json"), `{"name":"story-sandbox"}`)
	writeFile(t, filepath.Join(root, "web", "story", "apps", "web", "vite.config.ts"), "export default {}")
	// 库：有 package.json，没有 vite.config → 不是站点
	writeFile(t, filepath.Join(root, "web", "kit", "package.json"), `{"name":"kit"}`)
	// 目录名不合法（大写）→ 不是站点
	writeFile(t, filepath.Join(root, "web", "Bad", "package.json"), `{"name":"bad"}`)
	writeFile(t, filepath.Join(root, "web", "Bad", "vite.config.ts"), "export default {}")
	// 没有 package.json 的散目录 → 不是站点
	writeFile(t, filepath.Join(root, "web", "soft", "vite.config.ts"), "export default {}")
	return root
}

func TestDiscoverFrontends(t *testing.T) {
	sites, err := DiscoverFrontends(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sites {
		got = append(got, s.Name+"="+s.Out)
	}
	want := []string{"admin=web/admin/admin", "memory=web/memory/memory", "story=web/story/story"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sites = %v, want %v", got, want)
	}
}

func TestDiscoverFrontendsWithoutWeb(t *testing.T) {
	sites, err := DiscoverFrontends(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 0 {
		t.Fatalf("sites = %v, want empty", sites)
	}
}

func TestManifestIsJSONAndParsableByLine(t *testing.T) {
	root := fixture(t)
	content, err := Manifest(root)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Version int            `json:"version"`
		Sites   []FrontendSite `json:"sites"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("manifest 不是合法 JSON: %v\n%s", err, content)
	}
	if parsed.Version != ManifestVersion || len(parsed.Sites) != 3 {
		t.Fatalf("manifest = %+v", parsed)
	}

	// 脚本按行抓取：必须一个站点一行，且字段顺序固定。
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if lines[0] != "{" || !strings.Contains(lines[1], `"version": 1`) || lines[2] != `  "sites": [` {
		t.Fatalf("manifest 头部形状变了（scripts/frontends.sh 依赖它）:\n%s", content)
	}
	if lines[3] != `    {"name":"admin","dir":"web/admin","out":"web/admin/admin"},` {
		t.Fatalf("manifest 站点行形状变了（scripts/frontends.sh 依赖它）: %q", lines[3])
	}
}

func TestManifestEmptySites(t *testing.T) {
	content, err := Manifest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"version\": 1,\n  \"sites\": []\n}\n"
	if string(content) != want {
		t.Fatalf("manifest = %q, want %q", content, want)
	}
}

// 生成器把清单交给 protobuf 的生成响应（不是自己写盘）：这里证明非 .go 内容会原样落盘、
// 路径就是 ManifestPath（= gen/frontends.json 相对 protoc 输出目录）。
func TestManifestGoesThroughProtogenVerbatim(t *testing.T) {
	plugin, err := protogen.Options{}.New(&pluginpb.CodeGeneratorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	content, err := Manifest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.NewGeneratedFile(ManifestPath, "").Write(content); err != nil {
		t.Fatal(err)
	}
	resp := plugin.Response()
	if resp.GetError() != "" {
		t.Fatalf("response error: %s", resp.GetError())
	}
	if len(resp.File) != 1 {
		t.Fatalf("files = %d, want 1", len(resp.File))
	}
	if got := resp.File[0].GetName(); got != ManifestPath {
		t.Errorf("name = %q, want %q", got, ManifestPath)
	}
	if got := resp.File[0].GetContent(); got != string(content) {
		t.Errorf("content 被改写（应该原样透传）:\n%s", got)
	}
}

func TestIsSiteName(t *testing.T) {
	for _, name := range []string{"a", "app", "web-1", "a-b-c", strings.Repeat("a", 63)} {
		if !IsSiteName(name) {
			t.Errorf("IsSiteName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "-a", "a-", "a_b", "A", "a.b", strings.Repeat("a", 64)} {
		if IsSiteName(name) {
			t.Errorf("IsSiteName(%q) = true, want false", name)
		}
	}
}
