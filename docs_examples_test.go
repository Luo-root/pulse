package pulse

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocsExamplesUseCurrentSignatures 扫全仓 `*.md` 里 `New(` / `NewNode(` / `Sub(` 的
// 调用，对照当前签名看两件事：**实参够不够**、**描述位上放的是不是描述**。
//
// 为什么需要它：**文档里的代码块不参与编译**，所以签名一变，漏迁移的示例不会被
// `go test ./...` 发现——阶段一给这三个构造函数加 `description` 时，Go 侧 235 处调用点
// 都迁移了，README / 站点 / 设计文档里那几十处却漏了（评审在 #308 上点出来的那条）。
// 这条用例把那次「文本搜索复核」变成常驻闸门：下次改签名，它会直接点名要去改哪个文件。
//
// 判据不只有实参个数：旧签名漏掉 `description` 时，落到那个位置上的是**选项与绑定**——
//   - `New(ctx, id, pulse.WithObserver(obs))`   ← 选项落在描述位（实参个数照样是 3，只数个数看不见）
//   - `NewNode(id, nil, pulse.Provides(k), run)` ← `nil` 落在描述位
//   - `Sub(g, id, []pulse.SubBind{…}, child)`    ← 绑定切片落在描述位
//
// 所以描述位（`New` / `Sub` 的第 3 个、`NewNode` 的第 2 个）必须是字符串字面量
// 或变量，是 `nil` / 选项调用 / 复合字面量就报错。
//
// 判据还有第二层：描述位得是**能给出 string 的东西**。`123` / `1.5` / `0x1f` / `1e9` 与
// `true` / `false` / `iota` 看上去都「像标识符」，但一个是不合法标识符、一个是预声明常量，
// 放在这一位同样编译不过——只判「长得像标识符」会把它们当成合格的描述放过去。
// 正反两面的控制组在 TestDocsExampleGateNegativeControls，反例只活在用例里（写进文档
// 会把闸门自己弄红）。
//
// 带裸 `...` / `…` 的调用是教科书式的省略写法，跳过——它们本来就不打算被复制。
func TestDocsExamplesUseCurrentSignatures(t *testing.T) {
	const scanFloor = 20 // 扫到的调用少于这个数就说明扫描面坏了（用例自己会变成仪式）

	checked, elided, ignored := 0, 0, 0
	// 包目录就是仓库根（所有文档都在它下面）。
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != "." && (name == "node_modules" || name == "dist" || name == "testdata" ||
				isSyncedPkgDir(path) ||
				(strings.HasPrefix(name, ".") && name != ".github")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, c, e, ig := docExampleFindings(path, string(raw))
		checked, elided, ignored = checked+c, elided+e, ignored+ig
		for _, f := range found {
			t.Error(f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < scanFloor {
		t.Fatalf("只扫到 %d 处调用（跳过 %d 处省略写法、%d 处散文提及）——扫描面太窄，这条用例没在干活",
			checked, elided, ignored)
	}
}

// TestDocsExampleGateNegativeControls 给上面那条闸门自己上保险：**反例必须判红、正例必须
// 放过**。两面都断言，是为了让「一律判红」这种最省事的实现也过不了关——那种实现会让闸门
// 变成噪声源，下次改签名时没人再信它。
//
// 反例只活在这里：写进仓库文档的话，闸门当场就红了（这正是它要拦的东西）。
func TestDocsExampleGateNegativeControls(t *testing.T) {
	cases := []struct {
		name string
		doc  string // 合成的示例片段
		want string // 期望命中的判据片段；空串表示这段合规、不许报
	}{
		// —— 正例：描述位确实给得出 string ——
		{name: "字符串字面量", doc: `pulse.New(ctx, "demo", "演示图")`},
		{name: "裸字符串字面量", doc: "pulse.New(ctx, \"demo\", `演示图`)"},
		{name: "变量", doc: `pulse.New(ctx, "demo", desc)`},
		{name: "限定变量", doc: `pulse.NewNode("load", cfg.Desc, pulse.Provides(k), run)`},
		{name: "描述里带括号", doc: `pulse.New(ctx, "demo", "演示 (v2) 图")`},
		{name: "子图组合的描述变量", doc: `pulse.Sub(g, "child", desc, binds, child)`},

		// —— 反例：像标识符，却给不出 string ——
		{name: "整数", doc: `pulse.New(ctx, "demo", 123)`, want: "description 位"},
		{name: "浮点数", doc: `pulse.New(ctx, "demo", 1.5)`, want: "description 位"},
		{name: "十六进制", doc: `pulse.New(ctx, "demo", 0x1f)`, want: "description 位"},
		{name: "科学计数", doc: `pulse.New(ctx, "demo", 1e9)`, want: "description 位"},
		{name: "下划线分隔的数字", doc: `pulse.New(ctx, "demo", 1_000)`, want: "description 位"},
		{name: "布尔真", doc: `pulse.New(ctx, "demo", true)`, want: "description 位"},
		{name: "布尔假", doc: `pulse.NewNode("sum", false, pulse.Provides(k), run)`, want: "description 位"},
		{name: "iota", doc: `pulse.New(ctx, "demo", iota)`, want: "description 位"},

		// —— 反例：旧签名退化到这一位的东西 ——
		{name: "nil", doc: `pulse.NewNode("sum", nil, pulse.Provides(k), run)`, want: "description 位"},
		{name: "选项调用", doc: `pulse.New(ctx, "demo", pulse.WithObserver(obs))`, want: "description 位"},
		{name: "子图组合的绑定切片", doc: `pulse.Sub(g, "child", []pulse.SubBind{In(a, b)}, child)`, want: "description 位"},
		{name: "形参名占位", doc: `pulse.NewNode("sum", requires, provides, run)`, want: "description 位"},

		// —— 反例：实参根本不够 ——
		{name: "实参不够", doc: `pulse.New(ctx, "demo")`, want: "只给了 2 个实参"},
		{name: "子图组合实参不够", doc: `pulse.Sub(g, "child")`, want: "只给了 2 个实参"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, checked, _, _ := docExampleFindings("synthetic.md", tc.doc)
			if checked != 1 {
				t.Fatalf("合成片段里应当恰好扫到 1 处调用，实扫到 %d 处：%s", checked, tc.doc)
			}
			switch {
			case tc.want == "" && len(found) != 0:
				t.Fatalf("合法示例被判红：%s", found[0])
			case tc.want != "" && len(found) == 0:
				t.Fatalf("这条本该判红却没报：%s", tc.doc)
			case tc.want != "" && !strings.Contains(found[0], tc.want):
				t.Fatalf("报出的判据不对，期望含 %q，实际是：%s", tc.want, found[0])
			}
		})
	}
}

// ctorCall 只认**恰好**跟着左括号的那几个名字：`NewKey(` / `NewRecordObserver(` 这类
// 前缀相同的构造函数不会被误伤。
var ctorCall = regexp.MustCompile(`(pulse\.)?(New|NewNode|Sub)\(`)

// ctorDescAt = 三个构造函数里 description 的下标（0 起）：
// `New(ctx, id, description)` / `NewNode(id, description)` / `Sub(g, id, description, …)`。
var ctorDescAt = map[string]int{"New": 2, "NewNode": 1, "Sub": 2}

// docExampleFindings 扫一段 Markdown 文本，返回里面**不合当前签名**的示例（空切片即合规），
// 外带「实扫 / 省略写法 / 散文提及」三个计数。
//
// 抽成纯函数是为了能给闸门自己上保险：反例不能写进仓库文档（写进去闸门当场就红了），
// 只能拿合成文本喂进来——见 TestDocsExampleGateNegativeControls。
func docExampleFindings(path, text string) (findings []string, checked, elided, ignored int) {
	for _, loc := range ctorCall.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isIdentByte(text[loc[0]-1]) {
			continue // 长标识符的一部分（`xNew(`），不是这三个构造函数
		}
		call := text[loc[4]:loc[5]]
		args, frag, ok := splitCallArgs(text, loc[1]-1)
		line := 1 + strings.Count(text[:loc[0]], "\n")
		if !ok {
			// 括号不闭合 = 这只是一处**散文提及**（「`New(` / `NewNode(` / `Sub(`」
			// 这种罗列），不是调用——真示例的括号一定闭得上。跳过，别在这里误报；
			// 代价是把「示例真的漏了右括号」也一起放过，那不是这条用例要管的事。
			ignored++
			continue
		}
		if strings.Contains(frag, "...") || strings.Contains(frag, "…") {
			elided++
			continue
		}
		checked++
		at := ctorDescAt[call]
		switch {
		case len(args) <= at:
			findings = append(findings, fmt.Sprintf(
				"%s:%d：%s 只给了 %d 个实参（当前签名要求至少 %d 个，第 %d 个是 description）"+
					"——照抄这段示例会编译不过：%s", path, line, call, len(args), at+1, at+1, oneLine(frag)))
		case !looksLikeDescription(args[at]):
			findings = append(findings, fmt.Sprintf(
				"%s:%d：%s 的第 %d 个实参是 %s（description 位）——当前签名要求那里是描述，"+
					"旧签名遗留的选项 / 绑定退化到这里就会编译不过：%s",
				path, line, call, at+1, oneLine(args[at]), oneLine(frag)))
		}
	}
	return findings, checked, elided, ignored
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentByte(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// looksLikeDescription 判「这一位像不像 description」——**位置上放的能不能是 string**：
//
//   - 字符串字面量（`"demo"` / 裸串）与普通变量（`desc` / `cfg.Desc`）像；
//   - `nil`、选项调用（`pulse.WithObserver(…)`）、复合字面量（`[]pulse.SubBind{…}`）不像；
//   - 形参名占位符（`requires` / `provides` / `run` …）不像——那是旧签名留在这一位上的
//     东西（`NewNode("wait-forever", requires, provides, run, …)` 就这么来的），而示例里
//     真要传变量时用的名字不会恰好是形参名；
//   - **像标识符但给不出 string 的写法**也不像：数字字面量（`123` / `1.5` / `0x1f` / `1e9`
//     ——首字符就不是标识符起始位）与预声明常量 `true` / `false` / `iota`。
func looksLikeDescription(arg string) bool {
	s := strings.TrimSpace(arg)
	if s == "" || paramNames[s] || nonStringArgs[s] {
		return false
	}
	if s[0] == '"' || s[0] == '`' {
		return true
	}
	if !isIdentStart(s[0]) {
		return false // 数字字面量、负号、`.` 开头——都不可能求值成 string
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) && s[i] != '.' {
			return false
		}
	}
	return true
}

var paramNames = map[string]bool{
	"requires": true, "provides": true, "run": true, "binds": true,
	// 签名表里的形参写法（`pulse.NewNode(id, Requires, Provides, run)`）也是同一类占位。
	"Requires": true, "Provides": true, "Run": true,
}

// nonStringArgs = 长得像标识符、但求值结果不可能是 string 的那几个：`nil` 是无类型零值，
// `true` / `false` / `iota` 是预声明常量。
var nonStringArgs = map[string]bool{
	"nil": true, "true": true, "false": true, "iota": true,
}

// isSyncedPkgDir 认 `site/packages/<pkg>` 与 `site/en/packages/<pkg>`：那两个目录由
// `site/scripts/sync-docs.mjs` 从各包 README 生成（单源是仓库里的 README，已经被扫到），
// 是构建产物、不入库，本地残留一份旧的会让这条用例假红。
func isSyncedPkgDir(path string) bool {
	norm := filepath.ToSlash(path)
	for _, prefix := range []string{"site/packages/", "site/en/packages/"} {
		if rest := strings.TrimPrefix(norm, prefix); rest != norm {
			return !strings.Contains(rest, "/")
		}
	}
	return false
}

// splitCallArgs 从 `(` 起解析平衡的实参表，返回顶层实参（已去掉首尾空白）与整段文本。
// 跳过字符串 / 字节串 / 符文量与注释，所以示例注释里的括号不会把切分带偏。
func splitCallArgs(text string, open int) ([]string, string, bool) {
	var args []string
	depth, start := 0, open+1
	for i := open; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"' || c == '`':
			for i++; i < len(text); i++ {
				if text[i] == '\\' && c == '"' {
					i++
					continue
				}
				if text[i] == c {
					break
				}
			}
		case c == '\'':
			for i++; i < len(text); i++ {
				if text[i] == '\\' {
					i++
					continue
				}
				if text[i] == '\'' {
					break
				}
			}
		case c == '/' && i+1 < len(text) && text[i+1] == '/':
			for i++; i < len(text) && text[i] != '\n'; i++ {
			}
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			for i += 2; i+1 < len(text) && !(text[i] == '*' && text[i+1] == '/'); i++ {
			}
			i++
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
			if depth == 0 {
				last := strings.TrimSpace(text[start:i])
				if last != "" {
					args = append(args, last)
				}
				return args, text[open : i+1], true
			}
		case c == ',' && depth == 1:
			args = append(args, strings.TrimSpace(text[start:i]))
			start = i + 1
		}
	}
	return args, text[open:], false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
