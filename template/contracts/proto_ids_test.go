package contracts

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stringIDAllow 是「本来就该是 string」的 id 字段登记表：外部/技术标识与不透明句柄
// （渠道单号、SSO 客户端名、trace id…）可以是 string。
//
// 新增一条代表一次评审，理由写进值里；不要为了让测试变绿而加。
var stringIDAllow = map[string]string{
	// "traceId": "链路追踪 id，不是实体",
}

var (
	protoStringField  = regexp.MustCompile(`^\s*string\s+([A-Za-z_]\w*)\s*=`)
	protoMessageStart = regexp.MustCompile(`^\s*message\s+([A-Za-z_]\w*)\s*\{`)
	protoComment      = regexp.MustCompile(`//.*$`)
)

// TestProtoEntityIDsAreInt64 守框架 §5 的 ID 口径：实体主键及其外键/引用在 proto 里
// 必须是 int64——protojson 自动把它编成十进制字符串，ts-proto 再收窄成 EntityId，
// 前端不会掉精度。写成 `string orderId` 就退回到「字符串当业务 ID」的旧世界。
//
// 生成器只查「声明出来的整数必须是 int64」（int32/uint* 一律失败），拦不住
// `string orderId` 这类写法，所以这条守卫必须留在项目侧。
func TestProtoEntityIDsAreInt64(t *testing.T) {
	walkProtoStringFields(t, func(t *testing.T, file, message, field string) {
		if field != "id" && !strings.HasSuffix(field, "Id") {
			return
		}
		if _, ok := stringIDAllow[field]; ok {
			return
		}
		t.Errorf("%s: message %s 用 string %s；实体 ID 必须是 int64（protojson 自动输出字符串）", file, message, field)
	})
}

// walkProtoStringFields 遍历 proto/** 的 string 字段声明（已去掉行注释），把文件名、
// 所在消息与字段名交给回调。文件名相对 proto/ 根，失败信息里直接可定位。
//
// 一个文件都没扫到就失败：守卫因为目录改名而静默通过，比没有守卫更坏。
func walkProtoStringFields(t *testing.T, check func(t *testing.T, file, message, field string)) {
	t.Helper()
	scanned := 0
	err := filepath.WalkDir("../proto", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "../proto/")
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		scanned++

		message := ""
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := protoComment.ReplaceAllString(scanner.Text(), "")
			if m := protoMessageStart.FindStringSubmatch(line); m != nil {
				message = m[1]
			}
			if m := protoStringField.FindStringSubmatch(line); m != nil {
				check(t, rel, message, m[1])
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("没扫到任何 .proto：守卫的入口路径已失效（proto/ 被改名或移走了）")
	}
}
