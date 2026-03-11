// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Patterns for ServeMux routing.

package http

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// pattern 表示可以与 HTTP 请求进行匹配的模式。
// 它包含可选的请求方法、可选的主机名和路径。
type pattern struct {
	str    string // 原始字符串
	method string
	host   string
	// 路径的内部表示与表面语法不同，这样可以简化大多数算法。
	//
	// 以 '/' 结尾的路径用一个匿名的 "..." 通配符表示。
	// 例如，路径 "a/" 被表示为字面量段 "a" 后跟一个 multi==true 的段。
	//
	// 以 "{$}" 结尾的路径用字面量段 "/" 表示。
	// 例如，路径 "a/{$}" 被表示为字面量段 "a" 后跟字面量段 "/"。
	segments []segment
	loc      string // 注册调用的源码位置，用于生成有帮助的错误信息
}

func (p *pattern) String() string { return p.str }

func (p *pattern) lastSegment() segment {
	return p.segments[len(p.segments)-1]
}

// segment 是模式中的一个片段，可以匹配一个或多个路径段，或者尾部的斜杠。
//
// 若 wild 为 false，则匹配字面量路径段；若 s == "/"，则匹配尾部斜杠。
// 示例：
//
//	"a" => segment{s: "a"}
//	"/{$}" => segment{s: "/"}
//
// 若 wild 为 true 且 multi 为 false，则匹配单个路径段。
// 示例：
//
//	"{x}" => segment{s: "x", wild: true}
//
// 若 wild 和 multi 均为 true，则匹配所有剩余路径段。
// 示例：
//
//	"{rest...}" => segment{s: "rest", wild: true, multi: true}
type segment struct {
	s     string // 字面量或通配符名称，或 "/" 表示 "/{$}"
	wild  bool
	multi bool // "..." 通配符
}

// parsePattern parses a string into a Pattern.
// The string's syntax is
//
//	[METHOD] [HOST]/[PATH]
//
// where:
//   - METHOD is an HTTP method
//   - HOST is a hostname
//   - PATH consists of slash-separated segments, where each segment is either
//     a literal or a wildcard of the form "{name}", "{name...}", or "{$}".
//
// METHOD, HOST and PATH are all optional; that is, the string can be "/".
// If METHOD is present, it must be followed by at least one space or tab.
// Wildcard names must be valid Go identifiers.
// The "{$}" and "{name...}" wildcard must occur at the end of PATH.
// PATH may end with a '/'.
// Wildcard names in a path must be distinct.
//
// parsePattern 将路由字符串解析为结构化的 *pattern 对象。
// 字符串语法为：
//
//	[METHOD] [HOST]/[PATH]
//
// 各部分含义：
//   - METHOD：HTTP 方法（GET、POST 等），可选
//   - HOST：主机名，可选
//   - PATH：由 '/' 分隔的路径段组成，每段可以是字面量或以下三种通配符：
//     {name}    匹配单个路径段，捕获值可通过 r.PathValue("name") 获取
//     {name...} 贪婪匹配剩余所有路径段（必须位于 PATH 末尾）
//     {$}       精确匹配到结尾，等价于要求路径不含更多段（必须位于 PATH 末尾）
//
// METHOD、HOST、PATH 均可省略，最简合法 pattern 为 "/"。
// METHOD 若存在，其后必须跟至少一个空格或制表符。
// 通配符名称须是合法的 Go 标识符，且同一 PATH 内不能重复。
func parsePattern(s string) (_ *pattern, err error) {
	if len(s) == 0 {
		return nil, errors.New("empty pattern")
	}
	off := 0 // offset into string（当前解析偏移量，用于错误信息定位）
	// 统一在 err 上附加偏移量，方便调用者定位解析失败的位置
	defer func() {
		if err != nil {
			err = fmt.Errorf("at offset %d: %w", off, err)
		}
	}()

	// ── 第一步：分离 METHOD 和 HOST/PATH ──────────────────────────
	// 以第一个空格或制表符为分界点切割，左侧为 METHOD，右侧为 HOST/PATH。
	// 若没有空格，则整体作为 HOST/PATH，METHOD 为空。
	method, rest, found := s, "", false
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		method, rest, found = s[:i], strings.TrimLeft(s[i+1:], " \t"), true
	}
	if !found {
		rest = method
		method = ""
	}
	if method != "" && !validMethod(method) { //  校验方法名是否符合 RFC 2616 的 token 规则
		return nil, fmt.Errorf("invalid method %q", method)
	}
	p := &pattern{str: s, method: method}

	// ── 第二步：分离 HOST 和 PATH ─────────────────────────────────
	if found {
		off = len(method) + 1
	}
	// PATH 以第一个 '/' 为起点，'/' 之前的部分为 HOST
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return nil, errors.New("host/path missing /")
	}
	p.host = rest[:i]
	rest = rest[i:]
	// HOST 中不允许出现 '{'，否则可能是漏写了开头的 '/'
	if j := strings.IndexByte(p.host, '{'); j >= 0 {
		off += j
		return nil, errors.New("host contains '{' (missing initial '/'?)")
	}
	// At this point, rest is the path.
	off += i

	// 非 CONNECT 请求的路径会在匹配前经过 cleanPath 规范化，
	// 若注册时路径本身不干净，则永远无法被匹配到，直接报错。
	// An unclean path with a method that is not CONNECT can never match,
	// because paths are cleaned before matching.
	if method != "" && method != "CONNECT" && rest != cleanPath(rest) {
		return nil, errors.New("non-CONNECT pattern with unclean path can never match")
	}

	// ── 第三步：逐段解析 PATH ─────────────────────────────────────
	seenNames := map[string]bool{} // remember wildcard names to catch dups（记录已出现的通配符名，用于检测重复）
	for len(rest) > 0 {
		// Invariant: rest[0] == '/'.
		// 每次循环开头跳过 '/'，逐段处理
		rest = rest[1:]
		off = len(s) - len(rest)
		if len(rest) == 0 {
			// Trailing slash.
			// 路径以 '/' 结尾（如 "/static/"），等价于一个贪婪多段通配符，
			// 匹配该路径及其所有子路径。
			p.segments = append(p.segments, segment{wild: true, multi: true})
			break
		}
		// 取出下一个 '/' 之前的路径段
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			i = len(rest)
		}
		var seg string
		seg, rest = rest[:i], rest[i:]
		if i := strings.IndexByte(seg, '{'); i < 0 {
			// Literal.
			// 字面量段：URL 解码后直接存入，匹配时做精确字符串比较
			seg = pathUnescape(seg)
			p.segments = append(p.segments, segment{s: seg})
		} else {
			// Wildcard.
			// 通配符段：必须以 '{' 开头、'}' 结尾，中间为通配符名
			if i != 0 {
				return nil, errors.New("bad wildcard segment (must start with '{')")
			}
			if seg[len(seg)-1] != '}' {
				return nil, errors.New("bad wildcard segment (must end with '}')")
			}
			name := seg[1 : len(seg)-1]
			if name == "$" {
				// {$}：精确匹配到路径末尾，后面不能再有路径段
				if len(rest) != 0 {
					return nil, errors.New("{$} not at end")
				}
				p.segments = append(p.segments, segment{s: "/"})
				break
			}
			// 检查是否为 {name...} 贪婪通配符
			name, multi := strings.CutSuffix(name, "...")
			if multi && len(rest) != 0 {
				// {name...} 必须位于 PATH 最后一段
				return nil, errors.New("{...} wildcard not at end")
			}
			if name == "" {
				return nil, errors.New("empty wildcard")
			}
			// 通配符名须是合法 Go 标识符（字母/下划线开头，后接字母/数字/下划线）
			if !isValidWildcardName(name) {
				return nil, fmt.Errorf("bad wildcard name %q", name)
			}
			// 同一 pattern 内通配符名不能重复
			if seenNames[name] {
				return nil, fmt.Errorf("duplicate wildcard name %q", name)
			}
			seenNames[name] = true
			p.segments = append(p.segments, segment{s: name, wild: true, multi: multi})
		}
	}
	return p, nil
}

func isValidWildcardName(s string) bool {
	if s == "" {
		return false
	}
	// Valid Go identifier.
	for i, c := range s {
		if !unicode.IsLetter(c) && c != '_' && (i == 0 || !unicode.IsDigit(c)) {
			return false
		}
	}
	return true
}

func pathUnescape(path string) string {
	u, err := url.PathUnescape(path)
	if err != nil {
		// Invalidly escaped path; use the original
		return path
	}
	return u
}

// relationship 描述两个 pattern（p1 和 p2）之间的匹配关系。
type relationship string

const (
	equivalent   relationship = "equivalent"   // 两者匹配完全相同的请求集合
	moreGeneral  relationship = "moreGeneral"  // p1 匹配 p2 能匹配的所有请求，且还能匹配更多
	moreSpecific relationship = "moreSpecific" // p2 匹配 p1 能匹配的所有请求，且还能匹配更多
	disjoint     relationship = "disjoint"     // 不存在任何请求能同时被两者匹配（互斥）
	overlaps     relationship = "overlaps"     // 存在请求能同时被两者匹配，但两者互不包含（交叉）
)

// conflictsWith 判断 p1 与 p2 是否冲突，即是否存在某个请求同时被两者匹配，
// 但两者之间又没有明确的优先级高低。
//
// 优先级由以下两条规则决定：
//  1. 带有 host 的 pattern 优先于不带 host 的 pattern。
//  2. method 和 path 更具体的 pattern 优先级更高。若第二个 pattern 匹配的
//     (method, path) 对是第一个的超集，则第一个更具体。
//
// 若规则 1 不适用，则当两个 pattern 的关系为"等价"（匹配完全相同的请求集合）
// 或"交叉"（各自能匹配对方不能匹配的请求，但又有公共交集）时，认为它们冲突。
func (p1 *pattern) conflictsWith(p2 *pattern) bool {
	if p1.host != p2.host {
		// 两者 host 不同：要么一个有 host 一个没有（有 host 的按规则 1 获胜，不冲突），
		// 要么两者都有 host 但值不同（不可能匹配同一路径，不冲突）。
		return false
	}
	rel := p1.comparePathsAndMethods(p2)
	// 等价或交叉均构成冲突
	return rel == equivalent || rel == overlaps
}

// comparePathsAndMethods 综合比较两个 pattern 在 method 和 path 两个维度上的关系。
func (p1 *pattern) comparePathsAndMethods(p2 *pattern) relationship {
	mrel := p1.compareMethods(p2)
	// 优化：method 已经互斥，无需再比较 path，直接返回 disjoint。
	if mrel == disjoint {
		return disjoint
	}
	prel := p1.comparePaths(p2)
	// 将 method 关系与 path 关系合并，得到整体关系。
	return combineRelationships(mrel, prel)
}

// compareMethods 比较两个 pattern 在 method 部分的关系。
//
// method 有三种取值：空字符串、"GET" 或其他具体方法。
//   - 空字符串：匹配任意 method，是最宽泛的。
//   - "GET"：同时匹配 GET 和 HEAD（Go 路由规范：GET pattern 隐式处理 HEAD）。
//   - 其他：只匹配自身。
func (p1 *pattern) compareMethods(p2 *pattern) relationship {
	if p1.method == p2.method {
		// 完全相同，等价。
		return equivalent
	}
	if p1.method == "" {
		// p1 匹配任意 method，p2 有具体限定，p1 更宽泛。
		return moreGeneral
	}
	if p2.method == "" {
		// p2 匹配任意 method，p1 有具体限定，p1 更具体。
		return moreSpecific
	}
	if p1.method == "GET" && p2.method == "HEAD" {
		// p1 匹配 GET 和 HEAD，p2 只匹配 HEAD，p1 更宽泛。
		return moreGeneral
	}
	if p2.method == "GET" && p1.method == "HEAD" {
		// p2 匹配 GET 和 HEAD，p1 只匹配 HEAD，p1 更具体。
		return moreSpecific
	}
	// 两者都有具体 method 且互不包含，互斥。
	return disjoint
}

// comparePaths 比较两个 pattern 在 path 部分的关系。
func (p1 *pattern) comparePaths(p2 *pattern) relationship {
	// 优化：若两个 pattern 均不以多段通配符（"..."）结尾，则只有段数相同时才可能匹配
	// 相同的路径，段数不同直接互斥。
	if len(p1.segments) != len(p2.segments) && !p1.lastSegment().multi && !p2.lastSegment().multi {
		return disjoint
	}

	// 逐段对比两个 pattern 中对应位置的段，将每段的关系不断合并为整体关系。
	var segs1, segs2 []segment
	rel := equivalent
	for segs1, segs2 = p1.segments, p2.segments; len(segs1) > 0 && len(segs2) > 0; segs1, segs2 = segs1[1:], segs2[1:] {
		rel = combineRelationships(rel, compareSegments(segs1[0], segs2[0]))
		if rel == disjoint {
			// 某段已互斥，整体必然互斥，提前返回。
			return rel
		}
	}

	// 两个 pattern 的对应段都已比较完毕。
	// 若段数相同，前面的循环已得出最终关系，直接返回。
	if len(segs1) == 0 && len(segs2) == 0 {
		return rel
	}

	// 段数不同时，只有较短的那个以多段通配符结尾，才能匹配较长 pattern 的剩余部分。
	// 此时多段通配符比剩余的具体段更宽泛，将该"更宽泛"关系并入整体结果。
	if len(segs1) < len(segs2) && p1.lastSegment().multi {
		// p1 更短且以 multi 结尾，p1 比 p2 更宽泛。
		return combineRelationships(rel, moreGeneral)
	}
	if len(segs2) < len(segs1) && p2.lastSegment().multi {
		// p2 更短且以 multi 结尾，p1 比 p2 更具体。
		return combineRelationships(rel, moreSpecific)
	}
	// 段数不同且较短一方不以 multi 结尾，两者互斥。
	return disjoint
}

// compareSegments 比较两个路径段之间的关系。
// 段的类型从宽泛到具体依次为：多段通配符（multi）> 单段通配符（wild）> 字面量。
func compareSegments(s1, s2 segment) relationship {
	if s1.multi && s2.multi {
		// 两者都是多段通配符，等价。
		return equivalent
	}
	if s1.multi {
		// s1 是多段通配符，比 s2 更宽泛。
		return moreGeneral
	}
	if s2.multi {
		// s2 是多段通配符，s1 比 s2 更具体。
		return moreSpecific
	}
	if s1.wild && s2.wild {
		// 两者都是单段通配符，等价（均匹配任意单段）。
		return equivalent
	}
	if s1.wild {
		if s2.s == "/" {
			// 单段通配符不匹配尾部斜杠（即 {$} 段），互斥。
			return disjoint
		}
		// s1 是单段通配符，比字面量 s2 更宽泛。
		return moreGeneral
	}
	if s2.wild {
		if s1.s == "/" {
			// 字面量为尾部斜杠（{$} 段），单段通配符无法匹配，互斥。
			return disjoint
		}
		// s2 是单段通配符，s1 字面量比 s2 更具体。
		return moreSpecific
	}
	// 两者都是字面量，值相同则等价，否则互斥。
	if s1.s == s2.s {
		return equivalent
	}
	return disjoint
}

// combineRelationships 将两个局部关系合并为整体关系。
// 用于把 pattern 拆分为多个维度（如 method 和 path）分别比较后，再汇总出最终结论。
//
// 合并规则示例：
//   - 在某维度上 p1 更宽泛，在另一维度上等价 → 整体上 p1 更宽泛。
//   - 在某维度上 p1 更宽泛，在另一维度上 p1 更具体 → 两者交叉（overlaps）。
func combineRelationships(r1, r2 relationship) relationship {
	switch r1 {
	case equivalent:
		// r1 等价时，整体关系完全由 r2 决定。
		return r2
	case disjoint:
		// r1 已互斥，整体必然互斥。
		return disjoint
	case overlaps:
		if r2 == disjoint {
			// 任一维度互斥，整体互斥。
			return disjoint
		}
		// 已有交叉，再叠加任何非互斥关系仍是交叉。
		return overlaps
	case moreGeneral, moreSpecific:
		switch r2 {
		case equivalent:
			// r2 等价，整体关系由 r1 决定。
			return r1
		case inverseRelationship(r1):
			// 一个维度 r1 更宽泛，另一个维度 r1 更具体，方向相反 → 交叉。
			return overlaps
		default:
			// 两个维度方向一致（同为更宽泛或同为更具体），整体取 r2（与 r1 相同）。
			return r2
		}
	default:
		panic(fmt.Sprintf("unknown relationship %q", r1))
	}
}

// inverseRelationship 返回关系 r 的逆关系。
// 若 p1 对 p2 的关系是 r，则 p2 对 p1 的关系是 inverseRelationship(r)。
// equivalent 和 disjoint 的逆关系是自身。
func inverseRelationship(r relationship) relationship {
	switch r {
	case moreSpecific:
		return moreGeneral
	case moreGeneral:
		return moreSpecific
	default:
		return r
	}
}

// isLitOrSingle 判断一个段是否为"普通字面量"或"单段通配符"，
// 即排除多段通配符（multi）和表示路径终止的 {$}（s == "/"）。
func isLitOrSingle(seg segment) bool {
	if seg.wild {
		return !seg.multi
	}
	return seg.s != "/"
}

// describeConflict 返回两个冲突 pattern 的冲突原因描述，用于生成友好的错误信息。
func describeConflict(p1, p2 *pattern) string {
	mrel := p1.compareMethods(p2)
	prel := p1.comparePaths(p2)
	rel := combineRelationships(mrel, prel)
	if rel == equivalent {
		// 两者完全等价，匹配同一请求集合。
		return fmt.Sprintf("%s matches the same requests as %s", p1, p2)
	}
	if rel != overlaps {
		panic("describeConflict called with non-conflicting patterns")
	}
	if prel == overlaps {
		// path 部分交叉：举出公共路径、以及各自独有的路径示例，帮助开发者定位问题。
		return fmt.Sprintf(`%[1]s and %[2]s both match some paths, like %[3]q.
But neither is more specific than the other.
%[1]s matches %[4]q, but %[2]s doesn't.
%[2]s matches %[5]q, but %[1]s doesn't.`,
			p1, p2, commonPath(p1, p2), differencePath(p1, p2), differencePath(p2, p1))
	}
	if mrel == moreGeneral && prel == moreSpecific {
		// method 更宽泛但 path 更具体，两个维度方向相反，导致冲突。
		return fmt.Sprintf("%s matches more methods than %s, but has a more specific path pattern", p1, p2)
	}
	if mrel == moreSpecific && prel == moreGeneral {
		// method 更具体但 path 更宽泛，两个维度方向相反，导致冲突。
		return fmt.Sprintf("%s matches fewer methods than %s, but has a more general path pattern", p1, p2)
	}
	return fmt.Sprintf("bug: unexpected way for two patterns %s and %s to conflict: methods %s, paths %s", p1, p2, mrel, prel)
}

// writeMatchingPath 将 segs 中所有段对应的路径文本依次写入 b，
// 生成一个能被这些段匹配的示例路径字符串。
func writeMatchingPath(b *strings.Builder, segs []segment) {
	for _, s := range segs {
		writeSegment(b, s)
	}
}

// writeSegment 将单个段写入 b：始终先写 '/'，
// 若为多段通配符（multi）或 {$} 段（s == "/"）则只写斜杠，否则再追加段的文本内容。
func writeSegment(b *strings.Builder, s segment) {
	b.WriteByte('/')
	if !s.multi && s.s != "/" {
		b.WriteString(s.s)
	}
}

// commonPath 返回一个同时被 p1 和 p2 匹配的示例路径。
// 调用前提：此类路径确实存在。
func commonPath(p1, p2 *pattern) string {
	var b strings.Builder
	var segs1, segs2 []segment
	for segs1, segs2 = p1.segments, p2.segments; len(segs1) > 0 && len(segs2) > 0; segs1, segs2 = segs1[1:], segs2[1:] {
		if s1 := segs1[0]; s1.wild {
			// s1 是通配符，用 s2 的具体值填充，确保同时满足两者。
			writeSegment(&b, segs2[0])
		} else {
			// s1 是字面量，直接使用。
			writeSegment(&b, s1)
		}
	}
	// 处理段数不等的情况（较短一方以 multi 结尾）：
	// 追加较长一方剩余段的示例路径。
	if len(segs1) > 0 {
		writeMatchingPath(&b, segs1)
	} else if len(segs2) > 0 {
		writeMatchingPath(&b, segs2)
	}
	return b.String()
}

// differencePath 返回一个被 p1 匹配、但不被 p2 匹配的示例路径。
// 调用前提：此类路径确实存在。
func differencePath(p1, p2 *pattern) string {
	var b strings.Builder

	var segs1, segs2 []segment
	for segs1, segs2 = p1.segments, p2.segments; len(segs1) > 0 && len(segs2) > 0; segs1, segs2 = segs1[1:], segs2[1:] {
		s1 := segs1[0]
		s2 := segs2[0]
		if s1.multi && s2.multi {
			// 从此段开始两者匹配的路径集合相同，差异必然已在前面的段中体现，
			// 补一个 '/' 收尾即可。
			b.WriteByte('/')
			return b.String()
		}
		if s1.multi && !s2.multi {
			// s1 是多段通配符，s2 不是，说明 s1 能匹配更多路径。
			// 用尾部斜杠（空的多段匹配）来区分两者，
			// 但若 s2 是 {$} 段，则需要追加一个额外的路径段才能被 s1 匹配而不被 s2 匹配。
			b.WriteByte('/')
			if s2.s == "/" {
				if s1.s != "" {
					b.WriteString(s1.s) // 优先使用通配符名作为示例段
				} else {
					b.WriteString("x")
				}
			}
			return b.String()
		}
		if !s1.multi && s2.multi {
			// s2 是多段通配符而 s1 不是，直接写入 s1 的具体段。
			writeSegment(&b, s1)
		} else if s1.wild && s2.wild {
			// 两者都是单段通配符，填什么都能同时匹配，使用 s1 的通配符名。
			writeSegment(&b, s1)
		} else if s1.wild && !s2.wild {
			// s1 是通配符，s2 是字面量。
			// 任何不等于 s2.s 的值都能被 s1 匹配而不被 s2 匹配。
			// 优先用通配符名，若恰好与字面量相同则在字面量后追加 "x" 以示区分。
			if s1.s != s2.s {
				writeSegment(&b, s1)
			} else {
				b.WriteByte('/')
				b.WriteString(s2.s + "x")
			}
		} else if !s1.wild && s2.wild {
			// s1 是字面量，s2 是通配符，s2 能匹配 s1，直接用 s1 的字面量。
			writeSegment(&b, s1)
		} else {
			// 两者都是字面量。由于前提是两个 pattern 交叉（overlaps），
			// 对应位置的字面量必然相同，直接使用即可。
			if s1.s != s2.s {
				panic(fmt.Sprintf("literals differ: %q and %q", s1.s, s2.s))
			}
			writeSegment(&b, s1)
		}
	}
	if len(segs1) > 0 {
		// p1 比 p2 长，且 p2 不以 multi 结尾，追加 p1 剩余段即可满足 p1 而不满足 p2。
		writeMatchingPath(&b, segs1)
	} else if len(segs2) > 0 {
		writeMatchingPath(&b, segs2)
	}
	return b.String()
}
