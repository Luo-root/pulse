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
// 判据不只有实参个数：旧签名少一个 `description` 时，落到那个位置上的是**选项与绑定**——
//   - `New("demo", pulse.WithObserver(obs))`    ← 选项落在描述位（实参个数照样是 2，只数个数看不见）
//   - `NewNode(id, nil, pulse.Provides(k), run)` ← `nil` 落在描述位
//   - `Sub(g, id, []pulse.SubBind{…}, child)`    ← 绑定切片落在描述位
//
// 所以描述位（`New` / `NewNode` 的第 2 个、`Sub` 的第 3 个）必须是字符串字面量
// 或变量，是 `nil` / 选项调用 / 复合字面量就报错。
//
// 判据还有第二层：描述位得是**能给出 string 的东西**。`123` / `1.5` / `0x1f` / `1e9` 与
// `true` / `false` / `iota` 看上去都「像标识符」，但一个是不合法标识符、一个是预声明常量，
// 放在这一位同样编译不过——只判「长得像标识符」会把它们当成合格的描述放过去。
// 正反两面的控制组在 TestDocsExampleGateNegativeControls，反例只活在用例里（写进文档
// 会把闸门自己弄红）。
//
// 带裸 `...` / `…` 的调用是教科书式的省略写法，跳过——它们本来就不打算被复制。
//
// 阶段 2 又把扫描面扩到**本阶段改动的入口**——五条新判据，都尽量**精确**（宁少报、不多报）：
//
//   - `New` 的第一个实参是一段 ctx（`ctx` / `context.Background()` …）：阶段 2 把 ctx
//     从 `New` 上拿掉了——定义没有生命周期，一次运行在谁的取消域里跑由 `Run` /
//     `NewExecution` 那一刻决定。旧示例的第一个实参正是它，而「少了一个实参」这条判据
//     看不见它（旧形状反而**多**一个实参）。
//   - `X.Run(` 的实参少于两件：跑一次是 `g.Run(ctx, in)`，ctx 与这一次的输入缺一不可。
//     零实参的旧形状 `g.Run()` 与只给了 ctx 的 `g.Run(ctx)` 都落在这条上。
//   - 零实参的 `X.Start()` / `X.Wait()`：这两个现在挂在 `Execution` 上。判据排除几个
//     **执行句柄**的常用名字（`e` / `ex` / `exec` / `execution`）与 `sync.WaitGroup` 的
//     `wg`——那些名字上的零实参正是当前形状；写成 `g.Start()` / `graph.Wait()` 的是没迁的。
//     散文里把接收者**类型**写出来的（`(*Graph).Start()`）另认一遍：那里直说了是 `Graph`，
//     而 `Graph` 上已经没有这三个方法。
//   - `Seed(` / `SkipSeed(` / `SeedByName(` / `SkipSeedByName(`：第一个实参必须是
//     `&…`（一次运行的 `*Inputs`）。旧形状第一个实参是图（`pulse.Seed(g, k, v)`），
//     写歪了正好落在这条判据上。一个实参都没给的（散文里罗列 “ `Seed()` “）不判——
//     那条判据问的是「第一件放的是不是 `&in`」，没有第一件就无从问起。
//   - `SourceSeed`：这个概念随「外部来源不再是定义的一部分」一起删了（定义层只有
//     `SourceInput`）。但**只判能照抄的两处**——带包名的 `pulse.SourceSeed`，与代码块
//     里的任何写法；散文里提一句「不再是 `SourceSeed`」是历史说明（仓库里记 `SubFunc`
//     已删除也是这个写法），照抄不了，放过。
func TestDocsExamplesUseCurrentSignatures(t *testing.T) {
	// scanFloor 是自检下限：当前全仓实际扫到 91 处判据站点（阶段 2 扩完判据之后），
	// 60 留出「文档瘦身」的余量，同时能拦住「扫描面塌了」——比如某个目录被新加的跳过
	// 规则排除掉、或多条判据一起失效（那种情况下这条用例会退化成仪式，比没有还糟）。
	const scanFloor = 60

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
		{name: "字符串字面量", doc: `pulse.New("demo", "演示图")`},
		{name: "裸字符串字面量", doc: "pulse.New(\"demo\", `演示图`)"},
		{name: "变量", doc: `pulse.New("demo", desc)`},
		{name: "选项跟在描述之后", doc: `pulse.New("demo", "演示图", pulse.WithMaxRunning(4))`},
		{name: "限定变量", doc: `pulse.NewNode("load", cfg.Desc, pulse.Provides(k), run)`},
		{name: "描述里带括号", doc: `pulse.New("demo", "演示 (v2) 图")`},
		{name: "子图组合的描述变量", doc: `pulse.Sub(g, "child", desc, binds, child)`},

		// —— 反例：像标识符，却给不出 string ——
		{name: "整数", doc: `pulse.New("demo", 123)`, want: "description 位"},
		{name: "浮点数", doc: `pulse.New("demo", 1.5)`, want: "description 位"},
		{name: "十六进制", doc: `pulse.New("demo", 0x1f)`, want: "description 位"},
		{name: "科学计数", doc: `pulse.New("demo", 1e9)`, want: "description 位"},
		{name: "下划线分隔的数字", doc: `pulse.New("demo", 1_000)`, want: "description 位"},
		{name: "布尔真", doc: `pulse.New("demo", true)`, want: "description 位"},
		{name: "布尔假", doc: `pulse.NewNode("sum", false, pulse.Provides(k), run)`, want: "description 位"},
		{name: "iota", doc: `pulse.New("demo", iota)`, want: "description 位"},

		// —— 反例：旧签名退化到这一位的东西 ——
		{name: "nil", doc: `pulse.NewNode("sum", nil, pulse.Provides(k), run)`, want: "description 位"},
		{name: "选项调用", doc: `pulse.New("demo", pulse.WithObserver(obs))`, want: "description 位"},
		{name: "子图组合的绑定切片", doc: `pulse.Sub(g, "child", []pulse.SubBind{In(a, b)}, child)`, want: "description 位"},
		{name: "形参名占位", doc: `pulse.NewNode("sum", requires, provides, run)`, want: "description 位"},

		// —— 反例：ctx 还在 New 上（阶段 2 拿掉的那个参数）——
		{name: "New 的第一个实参是 ctx", doc: `pulse.New(ctx, "demo", "演示图")`, want: "ctx 已经不在这里了"},
		{name: "New 的第一个实参是 context.Background()",
			doc: `pulse.New(context.Background(), "demo", "演示图")`, want: "ctx 已经不在这里了"},
		{name: "New 的第一个实参是别的 ctx 变量", doc: `pulse.New(rootCtx, "demo", "演示图")`, want: "ctx 已经不在这里了"},

		// —— 反例：实参根本不够 ——
		{name: "实参不够", doc: `pulse.New("demo")`, want: "只给了 1 个实参"},
		{name: "子图组合实参不够", doc: `pulse.Sub(g, "child")`, want: "只给了 2 个实参"},

		// —— 阶段 2：生命周期调用 ——
		{name: "跑一次的新形状", doc: `if err := g.Run(ctx, in); err != nil { return err }`},
		{name: "分步控制的新形状", doc: `_ = e.Start()`},
		{name: "执行句柄的另一个名字", doc: `_ = execution.Wait()`},
		{name: "执行句柄的缩写", doc: `_ = exec.Wait()`},
		{name: "sync.WaitGroup 的 Wait", doc: `wg.Wait()`},
		{name: "类型写出来的 Execution.Start", doc: "`(*Execution).Start()`"},
		{name: "直接写类型名的 Execution.Start", doc: "`Execution.Start()` 提交全部节点"},
		{name: "零实参的 Run", doc: `if err := g.Run(); err != nil {`, want: "Run 只给了 0 个实参"},
		{name: "只给 ctx 的 Run", doc: `g.Run(ctx)`, want: "Run 只给了 1 个实参"},
		{name: "图上的 Start", doc: `g.Start()`, want: "现在挂在 Execution 上"},
		{name: "子图上的 Wait", doc: `child.Wait()`, want: "现在挂在 Execution 上"},
		{name: "类型写出来的 Graph.Start", doc: "`(*Graph).Start()`", want: "Graph 上没有 Start()"},
		{name: "类型写出来的 Graph.Run", doc: "`(*Graph).Run()`", want: "Graph 上没有 Run()"},

		// —— 阶段 2：种值入口的第一件 ——
		{name: "种值的新形状", doc: `_ = pulse.Seed(&in, Docs, []string{"a"})`},
		{name: "跳过的种值", doc: `_ = pulse.SkipSeed(&in, Optional)`},
		{name: "按名种值", doc: `_ = pulse.SeedByName(&in, reg, "k", "int", 1)`},
		{name: "旧形状：第一件是图", doc: `_ = pulse.Seed(g, Topic, "x")`, want: "第一个实参"},
		{name: "旧形状：第一件是名字", doc: `pulse.SkipSeed(Optional)`, want: "第一个实参"},

		// —— 阶段 2：已删除的 SourceSeed ——
		{name: "代码块里的 SourceSeed", doc: "```go\nkind := SourceSeed\n```", want: "SourceSeed 已经删除"},
		{name: "带包名的 SourceSeed", doc: "`pulse.SourceSeed`", want: "SourceSeed 已经删除"},
		{name: "散文里的历史说明", doc: "静态来源只有 `SourceInput`，不再是 `SourceSeed`"},
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
// `New(id, description, opts…)` / `NewNode(id, description, …)` / `Sub(g, id, description, …)`。
// `New` 那一项从 2 变成 1 是阶段 2 的事：ctx 从它的参数表里拿掉了（见文件头）。
var ctorDescAt = map[string]int{"New": 1, "NewNode": 1, "Sub": 2}

// 阶段 2 的三类新判据（来历见文件头）。
var (
	// memberCall 认 `接收者.方法(`：接收者只取最后一个点号前那一段（`a.b.Run(` 认作 `b`）。
	memberCall = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\.(Run|Start|Wait)\(`)
	// ptrMemberCall 认把接收者**类型**写出来的散文写法（`(*Graph).Start()`）——那种写法
	// 不经过变量名，按名字排除接收者的办法看不见它。
	ptrMemberCall = regexp.MustCompile(`\(\*([A-Za-z_][A-Za-z0-9_]*)\)\.(Run|Start|Wait)\(`)
	// seedCall 认四个「往这一次运行的输入里放值」的入口。长的写在前面，免得 `SeedByName`
	// 被 `Seed` 先截走。
	seedCall = regexp.MustCompile(`(pulse\.)?(SeedByName|SkipSeedByName|SkipSeed|Seed)\(`)
	// staleSourceSeed 认已删除的 `SourceSeed`（带不带包名都算同一处）。
	staleSourceSeed = regexp.MustCompile(`(pulse\.)?SourceSeed`)
)

// execSideNames 是「接收者写出来就是执行侧」的那些名字：执行句柄（`e` / `exec` …）、
// 直接写类型名 `Execution`，以及 `sync.WaitGroup` 的 `wg`。它们上面零实参的
// `Start()` / `Wait()` 正是当前形状，不能误报；`Graph` 不在里面——那正是要抓的旧形状。
var execSideNames = map[string]bool{
	"e": true, "ex": true, "exec": true, "execution": true,
	"Execution": true, "wg": true,
}

// docExampleFindings 扫一段 Markdown 文本，返回里面**不合当前签名 / 已删除**的示例
// （空切片即合规），外带「判据站点 / 省略写法 / 散文提及」三个计数。
//
// 抽成纯函数是为了能给闸门自己上保险：反例不能写进仓库文档（写进去闸门当场就红了），
// 只能拿合成文本喂进来——见 TestDocsExampleGateNegativeControls。
func docExampleFindings(path, text string) (findings []string, checked, elided, ignored int) {
	count := func(why string) {
		if why == "elided" {
			elided++
			return
		}
		ignored++
	}

	// —— 一、三个构造函数的 description 位 ——
	for _, loc := range ctorCall.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isIdentByte(text[loc[0]-1]) {
			continue // 长标识符的一部分（`xNew(`），不是这三个构造函数
		}
		call := text[loc[4]:loc[5]]
		args, frag, why := parseCall(text, loc[0], loc[1]-1)
		if why != "" {
			count(why)
			continue
		}
		checked++
		line, at := lineAt(text, loc[0]), ctorDescAt[call]
		switch {
		case len(args) <= at:
			findings = append(findings, fmt.Sprintf(
				"%s:%d：%s 只给了 %d 个实参（当前签名要求至少 %d 个，第 %d 个是 description）"+
					"——照抄这段示例会编译不过：%s", path, line, call, len(args), at+1, at+1, oneLine(frag)))
		case call == "New" && looksLikeContext(args[0]):
			findings = append(findings, fmt.Sprintf(
				"%s:%d：New 的第一个实参是 %s——ctx 已经不在这里了（第一个参数是 graphID）："+
					"定义没有生命周期，一次运行在谁的取消域里跑由 Run(ctx, in) / NewExecution(ctx, in) "+
					"那一刻决定——照抄这段会编译不过：%s",
				path, line, oneLine(args[0]), oneLine(frag)))
		case !looksLikeDescription(args[at]):
			findings = append(findings, fmt.Sprintf(
				"%s:%d：%s 的第 %d 个实参是 %s（description 位）——当前签名要求那里是描述，"+
					"旧签名遗留的选项 / 绑定退化到这里就会编译不过：%s",
				path, line, call, at+1, oneLine(args[at]), oneLine(frag)))
		}
	}

	// —— 二、生命周期调用：`Run` 要两件实参，`Start` / `Wait` 挂在执行句柄上 ——
	for _, loc := range memberCall.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isIdentByte(text[loc[0]-1]) {
			continue // 长标识符的一部分（`myg.Run(`）
		}
		recv, method := text[loc[2]:loc[3]], text[loc[4]:loc[5]]
		args, frag, why := parseCall(text, loc[0], loc[1]-1)
		if why != "" {
			count(why)
			continue
		}
		checked++
		switch {
		case method == "Run" && len(args) < 2:
			findings = append(findings, fmt.Sprintf(
				"%s:%d：Run 只给了 %d 个实参（现在跑一次要 ctx 与这一次的输入两件：g.Run(ctx, in)）"+
					"——照抄这段会编译不过：%s", path, lineAt(text, loc[0]), len(args), oneLine(frag)))
		case method != "Run" && len(args) == 0 && !execSideNames[recv]:
			findings = append(findings, fmt.Sprintf(
				"%s:%d：%s() 现在挂在 Execution 上（%s 不是执行句柄）：跑一次是 g.Run(ctx, in)，"+
					"要分步就 e := g.NewExecution(ctx, in) 之后 e.Start() / e.Wait()"+
					"——照抄这段会编译不过：%s", path, lineAt(text, loc[0]), method, recv, oneLine(frag)))
		}
	}

	// —— 三、把接收者类型写出来的散文写法：`(*Graph).Start()` ——
	for _, loc := range ptrMemberCall.FindAllStringSubmatchIndex(text, -1) {
		_, frag, why := parseCall(text, loc[0], loc[1]-1)
		if why != "" {
			count(why)
			continue
		}
		checked++
		if text[loc[2]:loc[3]] != "Graph" {
			continue // `(*Execution).Start()` 正是当前形状；别的类型不归这条判据管
		}
		findings = append(findings, fmt.Sprintf(
			"%s:%d：Graph 上没有 %s()：定义侧只剩 Validate，跑一次的入口是 g.Run(ctx, in)，"+
				"分步控制挂在 Execution 上（e.Start() / e.Wait()）——照抄这段会编译不过：%s",
			path, lineAt(text, loc[0]), text[loc[4]:loc[5]], oneLine(frag)))
	}

	// —— 四、种值入口的第一件：`&…`（这一次运行的 `*Inputs`）——
	for _, loc := range seedCall.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isIdentByte(text[loc[0]-1]) {
			continue // 长标识符的一部分（`plan.Seed(`）
		}
		args, frag, why := parseCall(text, loc[0], loc[1]-1)
		if why != "" {
			count(why)
			continue
		}
		checked++
		// 只问「第一件放的是不是 &in」：一个实参都没给的（散文里罗列 `` `Seed()` ``）不判——
		// 没有第一件就无从问起，报了只会是噪声。
		if len(args) == 0 || strings.HasPrefix(strings.TrimSpace(args[0]), "&") {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"%s:%d：%s 的第一个实参是 %s——这里要的是**这一次运行的输入**（写成 `&in`："+
				"pulse.Seed(&in, k, v)），旧形状第一件是图（pulse.Seed(g, k, v)）"+
				"——照抄这段会编译不过：%s",
			path, lineAt(text, loc[0]), text[loc[4]:loc[5]], oneLine(args[0]), oneLine(frag)))
	}

	// —— 五、已删除的 `SourceSeed`：只判能照抄的那两处 ——
	for _, loc := range staleSourceSeed.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isIdentByte(text[loc[0]-1]) {
			continue
		}
		if loc[1] < len(text) && isIdentByte(text[loc[1]]) {
			continue // `SourceSeeds` 这样的长标识符
		}
		checked++
		if !strings.HasPrefix(text[loc[0]:loc[1]], "pulse.") && !insideFence(text, loc[0]) {
			continue // 散文里的历史说明：照抄不了，放过（见文件头）
		}
		findings = append(findings, fmt.Sprintf(
			"%s:%d：SourceSeed 已经删除——外部来源不再是定义的一部分（进口由 DeclareInput 声明、"+
				"值由 Seed(&in, k, v) 给，定义层的来源只有 SourceInput）——照抄这段会编译不过：%s",
			path, lineAt(text, loc[0]), text[loc[0]:loc[1]]))
	}
	return findings, checked, elided, ignored
}

// parseCall 解析 start 处那一次调用的实参表，并按「该不该判」分三类：
//
//   - `why == ""`：正常调用，照判，实参与**整段调用文本**（含接收者，报文里要引它）都给出来；
//   - `why == "elided"`：教科书式省略写法（`…` / `...`），本来就不打算被复制；
//   - `why == "ignored"`：括号不闭合——那是**散文里的罗列**（“ `New(` “ 这种），
//     不是调用。真示例的括号一定闭得上；代价是把「示例真的漏了右括号」也一起放过，
//     那不是这条闸门要管的事。
func parseCall(text string, start, open int) (args []string, frag, why string) {
	args, inner, ok := splitCallArgs(text, open)
	frag = text[start : open+len(inner)]
	if !ok {
		return nil, frag, "ignored"
	}
	if strings.Contains(inner, "...") || strings.Contains(inner, "…") {
		return nil, frag, "elided"
	}
	return args, frag, ""
}

// lineAt 是 pos 所在的行号（1 起）——报错要点得到文件里的位置。
func lineAt(text string, pos int) int { return 1 + strings.Count(text[:pos], "\n") }

// insideFence 判 pos 是不是落在 ``` 围栏之间：判据用它把「代码块里能照抄的引用」与
// 「散文里的历史说明」分开。
func insideFence(text string, pos int) bool {
	return strings.Count(text[:pos], "```")%2 == 1
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

// looksLikeContext 判这一位像不像一个 `context.Context`——`ctx` / `rootCtx` /
// `context.Background()` / `context.TODO()` / `rc.Context()`。
//
// 阶段 2 把 ctx 从 `New` 上拿掉了，旧示例的第一个实参正是它；而「实参够不够」这条判据
// 看不见它（旧形状是**多**一个实参，不是少一个）。
func looksLikeContext(arg string) bool {
	s := strings.TrimSpace(arg)
	return s != "" && (strings.HasSuffix(s, "ctx") || strings.HasSuffix(s, "Ctx") ||
		strings.HasPrefix(s, "context.") || strings.HasSuffix(s, ".Context()"))
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
