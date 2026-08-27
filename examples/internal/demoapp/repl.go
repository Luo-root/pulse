package demoapp

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Luo-root/pulse/llm"
)

// LineSource 是 stdin 的唯一行缓冲持有者：REPL 主循环与 interactive
// 审批器共享同一实例，顺序消费同一 bufio.Reader，从结构上消除
// 「两个 reader 各自预读、互相抢行」的问题。读取必须发生在同一个
// goroutine 链上（loop 回合内工具串行执行已满足）。
type LineSource struct {
	br *bufio.Reader
}

// NewLineSource 包装任意输入流；重复包装同一 LineSource 时原样返回。
func NewLineSource(r io.Reader) *LineSource {
	if ls, ok := r.(*LineSource); ok {
		return ls
	}
	return &LineSource{br: bufio.NewReader(r)}
}

// Read 让 *LineSource 满足 io.Reader（供既有接口透传）。
func (l *LineSource) Read(p []byte) (int, error) { return l.br.Read(p) }

// ReadLine 读一行并去掉行尾 \r\n。EOF 且无残余字节时返回 ("", io.EOF)；
// 文件尾残留的最后一行（无换行符）返回内容与 nil。
func (l *LineSource) ReadLine() (string, error) {
	line, err := l.br.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Command 是 REPL 解析后的一条指令。
type Command struct {
	Kind    string // text, image, file, send, clear, history, help, exit, empty, unknown
	Text    string
	Path    string
	MIME    string
	Unknown string
}

// ParseCommand 解析一行用户输入。普通文字立即构成一轮；斜杠命令用来附带多模态内容。
func ParseCommand(line string) Command {
	line = strings.TrimPrefix(line, "\uFEFF")
	line = strings.TrimSpace(line)
	if line == "" {
		return Command{Kind: "empty"}
	}
	if !strings.HasPrefix(line, "/") {
		return Command{Kind: "text", Text: line}
	}
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		return Command{Kind: "exit"}
	case "/help":
		return Command{Kind: "help"}
	case "/clear":
		return Command{Kind: "clear"}
	case "/history":
		return Command{Kind: "history"}
	case "/send":
		return Command{Kind: "send"}
	case "/image":
		if len(fields) < 2 {
			return Command{Kind: "unknown", Unknown: "/image 需要 URL 或本地路径"}
		}
		return Command{Kind: "image", Path: fields[1]}
	case "/file":
		if len(fields) < 2 {
			return Command{Kind: "unknown", Unknown: "/file 需要 URL 或本地路径"}
		}
		cmd := Command{Kind: "file", Path: fields[1]}
		if len(fields) >= 3 {
			cmd.MIME = fields[2]
		}
		return cmd
	default:
		return Command{Kind: "unknown", Unknown: "未知命令 " + fields[0]}
	}
}

// HelpText 是 REPL 帮助。
func HelpText() string {
	return strings.Join([]string{
		"直接输入文字并回车：立即发送本轮",
		"/image <URL 或本地路径>    附加图片，下一次发送时带上",
		"/file  <URL 或本地路径> [MIME]  附加 PDF/音频/视频",
		"/send                    发送当前已附加的媒体（可无文字）",
		"/clear                   清空未发送的附件",
		"/history                 显示已保存的对话消息数",
		"/help                    显示本帮助",
		"/exit                    退出",
	}, "\n")
}

// Draft 是尚未发送的一轮输入。
type Draft struct {
	Text        string
	Attachments []Attachment
}

// AddImage 把图片 URL 或本地文件加入草稿。
func (d *Draft) AddImage(path string) error {
	att, err := attachmentFromPath(path, "")
	if err != nil {
		return err
	}
	if att.MediaType == "" {
		att.MediaType = "image/png"
	}
	d.Attachments = append(d.Attachments, att)
	return nil
}

// AddFile 把开放模态附件加入草稿。
func (d *Draft) AddFile(path, mime string) error {
	att, err := attachmentFromPath(path, mime)
	if err != nil {
		return err
	}
	d.Attachments = append(d.Attachments, att)
	return nil
}

func attachmentFromPath(path, mime string) (Attachment, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Attachment{}, fmt.Errorf("demo: empty media path")
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if mime == "" {
			mime = guessMediaType(path)
		}
		return Attachment{MediaType: mime, URL: path}, nil
	}
	data, mediaType, err := readMedia(path, mime)
	if err != nil {
		return Attachment{}, err
	}
	return Attachment{MediaType: mediaType, Data: data}, nil
}

// Input 把草稿转成 demo Input。
func (d Draft) Input() Input {
	in := Input{Text: d.Text, Attachments: append([]Attachment(nil), d.Attachments...)}
	return in
}

// Ready 表示草稿是否足以发送。
func (d Draft) Ready() bool {
	return strings.TrimSpace(d.Text) != "" || len(d.Attachments) > 0
}

// Loop 驱动交互循环。onTurn 返回本轮模型产出的消息，由调用方决定是否写入 history。
// in 必须传 NewLineSource 包装后的同一实例给 interactive 审批器（见 InstallHITL）。
func Loop(in io.Reader, out io.Writer, onTurn func(msg *llm.Message) ([]*llm.Message, error), historyLen func() int) error {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	fmt.Fprintln(out, HelpText())
	fmt.Fprintln(out, "输入文字开始对话。")
	lines := NewLineSource(in)
	var draft Draft
	for {
		fmt.Fprint(out, "> ")
		line, err := lines.ReadLine()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		cmd := ParseCommand(line)
		switch cmd.Kind {
		case "empty":
			continue
		case "exit":
			fmt.Fprintln(out, "bye")
			return nil
		case "help":
			fmt.Fprintln(out, HelpText())
		case "clear":
			draft = Draft{}
			fmt.Fprintln(out, "已清空未发送附件")
		case "history":
			n := 0
			if historyLen != nil {
				n = historyLen()
			}
			fmt.Fprintf(out, "history=%d\n", n)
		case "unknown":
			fmt.Fprintln(out, cmd.Unknown)
		case "image":
			if err := draft.AddImage(cmd.Path); err != nil {
				fmt.Fprintf(out, "附加图片失败: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "已附加图片 %s，输入文字或 /send 发送\n", cmd.Path)
		case "file":
			if err := draft.AddFile(cmd.Path, cmd.MIME); err != nil {
				fmt.Fprintf(out, "附加文件失败: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "已附加文件 %s，输入文字或 /send 发送\n", cmd.Path)
		case "text":
			draft.Text = cmd.Text
			if err := sendDraft(out, &draft, onTurn); err != nil {
				fmt.Fprintf(out, "本轮失败: %v\n", err)
			}
		case "send":
			if err := sendDraft(out, &draft, onTurn); err != nil {
				fmt.Fprintf(out, "本轮失败: %v\n", err)
			}
		}
	}
}

func sendDraft(out io.Writer, draft *Draft, onTurn func(msg *llm.Message) ([]*llm.Message, error)) error {
	if !draft.Ready() {
		fmt.Fprintln(out, "没有可发送的文字或附件")
		return nil
	}
	msg, err := draft.Input().Message()
	if err != nil {
		return err
	}
	produced, err := onTurn(msg)
	*draft = Draft{}
	if err != nil {
		return err
	}
	_ = produced
	return nil
}
