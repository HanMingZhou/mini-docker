// Package build 实现极简版 Dockerfile 解析与 build。
//
// 支持的指令：
//   - FROM <image>[:tag]              基础镜像
//   - RUN <shell command>             在当前层起容器执行命令，结果 commit 为新层
//   - COPY <src> <dst>                从构建上下文拷贝文件到当前层（不起容器）
//   - ADD <src> <dst>                 等价 COPY（不支持 URL / 自动解压）
//   - ENV KEY VALUE / ENV KEY=VALUE   加到 image config
//   - WORKDIR <path>                  设默认工作目录
//   - CMD <shell cmd>                 设默认 CMD（shell form 或 JSON array form 都支持）
//   - ENTRYPOINT <cmd>                设默认 ENTRYPOINT
//   - USER <name>                     记录但不实际切换
//
// 不支持：ARG、ONBUILD、HEALTHCHECK、STOPSIGNAL、LABEL 等高级指令；MAINTAINER 忽略。
// 也不支持多阶段构建。
package build

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Instruction 是 Dockerfile 里的一条指令。
type Instruction struct {
	Op   string   // 大写的指令名：FROM / RUN / COPY 等
	Args []string // 原始参数，按空白分割，或单元素（shell form 的命令串）
	Raw  string   // 原样的指令参数字符串（给 shell form 的 RUN/CMD 用）
}

// Parse 把一个 Dockerfile 的文本解析成指令序列。
// 处理行续接（反斜杠结尾）、空行和 `#` 开头的注释。
func Parse(r io.Reader) ([]Instruction, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var (
		insts   []Instruction
		pending strings.Builder
	)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if pending.Len() == 0 {
				continue
			}
		}
		if strings.HasSuffix(trimmed, "\\") {
			pending.WriteString(strings.TrimSuffix(trimmed, "\\"))
			pending.WriteByte(' ')
			continue
		}
		pending.WriteString(trimmed)
		full := strings.TrimSpace(pending.String())
		pending.Reset()
		if full == "" {
			continue
		}

		// Split into op + rest
		parts := strings.SplitN(full, " ", 2)
		op := strings.ToUpper(parts[0])
		rest := ""
		if len(parts) == 2 {
			rest = strings.TrimSpace(parts[1])
		}

		inst := Instruction{Op: op, Raw: rest}
		if rest != "" {
			inst.Args = strings.Fields(rest)
		}
		insts = append(insts, inst)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read dockerfile: %w", err)
	}
	return insts, nil
}

// ParseJSONArray 尝试把 `["a","b","c"]` 解析成字符串切片。
// 失败返回 nil，调用方应回落到 shell form。
func ParseJSONArray(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// ParseEnvAssign 解析 `KEY=VALUE` 或 `KEY VALUE` 形式。失败返回 "", "", false。
// 只处理单个键值对；多个的由上层按 shell 规则（引号）处理，本简化版不支持。
func ParseEnvAssign(s string) (key, value string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	// KEY=VALUE form（没空格）
	if i := strings.IndexByte(s, '='); i > 0 {
		firstSpace := strings.IndexByte(s, ' ')
		if firstSpace < 0 || i < firstSpace {
			return s[:i], s[i+1:], true
		}
	}
	// KEY VALUE form
	parts := strings.SplitN(s, " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], strings.TrimSpace(parts[1]), true
}
