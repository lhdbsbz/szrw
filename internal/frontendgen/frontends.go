package frontendgen

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// 前端站点清单（gen/frontends.json）。
//
// 构建脚本要回答一个问题：「这个仓有哪些前端站点，产物在哪」。
// 这个谓词以前在 build.ps1 / build.sh / ci-build.sh / web_dist.go 各写一份，
// 改一条规则要改四处，而且总有一处忘改。
//
// 现在只有两份实现，且都以本文件的规则为准：
//
//   - 本文件（Go）：生成器据此写 gen/frontends.json
//   - scripts/frontends.sh（shell）：脚本 source 它，读清单，清单缺失时才回退探测
//
// 清单是生成物（随 gen/ 一起被原子替换），不是配置项：不一致只说明没跑生成，
// 不存在「配置里写一个、磁盘上另一个」的第二种事实。
const (
	// WebRoot 是前端站点根目录（相对仓库根）。
	WebRoot = "web"
	// ManifestPath 是清单路径（相对 protoc 工作目录 = 仓库根，与 gen/ 其他生成物一致）。
	ManifestPath = "gen/frontends.json"
	// ManifestVersion 是清单格式版本。脚本按行解析（一站点一行），
	// 改字段顺序/引号风格必须同时改 scripts/frontends.sh。
	ManifestVersion = 1
	// siteNameMaxLen 与服务端站点路由的校验一致（等于一个 DNS label 的上限）。
	siteNameMaxLen = 63
)

// FrontendSite 是一个前端站点：工程目录 web/<name>，产物目录 web/<name>/<name>。
// 产物目录是框架常量（vite 的 outDir = 站点名），不由项目配置。
type FrontendSite struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Out  string `json:"out"`
}

// IsSiteName 判断目录名是否是合法站点名（目录名 = 子域名）。
// 规则与服务端的站点路由一致：小写字母/数字，中划线只能夹在中间。
func IsSiteName(name string) bool {
	if name == "" || len(name) > siteNameMaxLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		if c == '-' && i > 0 && i < len(name)-1 {
			continue
		}
		return false
	}
	return true
}

// IsFrontendDir 是「web/<name> 是一个前端站点」的唯一谓词：
// 有 package.json，且根目录或 apps/*/ 下有 vite.config.{ts,js}。
//
// 用 vite.config 而不是 package.json 的 name/scripts：web/kit 这类库也有 package.json，
// 用 name 或脚本名判会把库当成站点。多站点（apps/*）是同一谓词的第二种形状。
func IsFrontendDir(dir string) bool {
	if !isRegularFile(filepath.Join(dir, "package.json")) {
		return false
	}
	if hasViteConfig(dir) {
		return true
	}
	appsDir := filepath.Join(dir, "apps")
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && hasViteConfig(filepath.Join(appsDir, e.Name())) {
			return true
		}
	}
	return false
}

func hasViteConfig(dir string) bool {
	return isRegularFile(filepath.Join(dir, "vite.config.ts")) ||
		isRegularFile(filepath.Join(dir, "vite.config.js"))
}

func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// DiscoverFrontends 枚举 repoRoot/web/* 下的站点，按名字排序。
// web/ 不存在不是错误：后端-only 项目的清单就是空列表（脚本因此永远不用猜）。
func DiscoverFrontends(repoRoot string) ([]FrontendSite, error) {
	if repoRoot == "" {
		repoRoot = "."
	}
	webRoot := filepath.Join(repoRoot, WebRoot)
	entries, err := os.ReadDir(webRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", webRoot, err)
	}

	sites := make([]FrontendSite, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !IsSiteName(name) {
			continue
		}
		dir := path.Join(WebRoot, name)
		if !IsFrontendDir(filepath.Join(repoRoot, filepath.FromSlash(dir))) {
			continue
		}
		sites = append(sites, FrontendSite{Name: name, Dir: dir, Out: path.Join(dir, name)})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Name < sites[j].Name })
	return sites, nil
}

// Manifest 渲染 gen/frontends.json 的内容。
//
// 刻意手工排版成「一个站点一行、字段顺序固定」：Bash 没有 JSON 解析器，
// 但 sed 能稳定抓取这一种形状。名字已用 IsSiteName 约束过（无引号/反斜杠），
// 所以不需要转义就一定是合法 JSON。
func Manifest(repoRoot string) ([]byte, error) {
	sites, err := DiscoverFrontends(repoRoot)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"version\": %d,\n", ManifestVersion)
	if len(sites) == 0 {
		b.WriteString("  \"sites\": []\n}\n")
		return []byte(b.String()), nil
	}
	b.WriteString("  \"sites\": [\n")
	for i, s := range sites {
		if !IsSiteName(s.Name) {
			return nil, fmt.Errorf("站点名不合法: %q", s.Name)
		}
		fmt.Fprintf(&b, "    {\"name\":\"%s\",\"dir\":\"%s\",\"out\":\"%s\"}", s.Name, s.Dir, s.Out)
		if i < len(sites)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  ]\n}\n")
	return []byte(b.String()), nil
}
