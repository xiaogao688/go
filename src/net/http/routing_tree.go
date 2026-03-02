// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements a decision tree for fast matching of requests to
// patterns.
// 本文件实现了一棵决策树，用于将请求快速匹配到对应的 pattern。
//
// The root of the tree branches on the host of the request.
// 树的根节点按请求的 host 分支。
// The next level branches on the method.
// 第二层按 HTTP method 分支。
// The remaining levels branch on consecutive segments of the path.
// 之后各层按路径的连续段逐层分支。
//
// The "more specific wins" precedence rule can result in backtracking.
// "更具体的优先"这一优先级规则可能导致回溯。
// For example, given the patterns
// 例如，给定如下两个 pattern：
//
//	/a/b/z
//	/a/{x}/c
//
// we will first try to match the path "/a/b/c" with /a/b/z, and
// when that fails we will try against /a/{x}/c.
// 匹配路径 "/a/b/c" 时，会先尝试与 /a/b/z 匹配，失败后再尝试与 /a/{x}/c 匹配。

package http

import (
	"strings"
)

// A routingNode is a node in the decision tree.
// The same struct is used for leaf and interior nodes.
//
// routingNode 是决策树中的一个节点，叶节点和内部节点共用同一结构体。
//
//   - 叶节点（leaf node）：pattern 和 handler 均非 nil，代表一条已注册的路由规则。
//   - 内部节点（interior node）：pattern 和 handler 为 nil，仅作为分支跳转使用。
type routingNode struct {
	// A leaf node holds a single pattern and the Handler it was registered
	// with.
	// 叶节点存储对应的路由 pattern 及其 Handler。
	pattern *pattern
	handler Handler

	// An interior node maps parts of the incoming request to child nodes.
	// special children keys:
	//     "/"	trailing slash (resulting from {$})
	//	   ""   single wildcard
	//
	// 内部节点将请求的某一部分映射到子节点。
	// children 的特殊 key：
	//   "/"  —— 尾部斜线（来自 pattern 中的 {$}）
	//   ""   —— 单段通配符（来自 {name} 形式的通配符）
	children   mapping[string, *routingNode]
	multiChild *routingNode // child with multi wildcard; 多段通配符子节点（来自 {name...}）
	emptyChild *routingNode // optimization: child with key ""; 单段通配符子节点的快速访问缓存（key 为 ""）
}

// addPattern adds a pattern and its associated Handler to the tree
// at root.
// addPattern 将一个 pattern 及其对应的 Handler 插入以 root 为根的决策树。
//
// 树的层级结构固定为三层：
//
//	root
//	 └─ host 子节点        （第 1 层，按 Host 头分支）
//	      └─ method 子节点  （第 2 层，按 HTTP Method 分支）
//	           └─ 路径段子节点...（第 3 层起，按 PATH 各段逐层分支）
//
// 同一 host/method 前缀可以被多个 pattern 共享，不会重复创建。
func (root *routingNode) addPattern(p *pattern, h Handler) {
	// First level of tree is host.
	// 按 HOST 建立第一层子节点（空字符串表示无 host 限制的通用路由）
	n := root.addChild(p.host)
	// Second level of tree is method.
	// 按 METHOD 建立第二层子节点（空字符串表示不限制 HTTP 方法）
	n = n.addChild(p.method)
	// Remaining levels are path.
	// 将 PATH 各段递归插入剩余层级，最终叶节点存储 pattern 和 handler
	n.addSegments(p.segments, p, h)
}

// addSegments adds the given segments to the tree rooted at n.
// If there are no segments, then n is a leaf node that holds
// the given pattern and handler.
//
// addSegments 将路径段列表 segs 递归插入以 n 为根的子树。
// 每次处理 segs[0]，根据段的类型选择不同的插入策略：
//
//   - 多段通配符（multi，如 {rest...}）：必须是最后一段，存入 n.multiChild。
//   - 单段通配符（wild，如 {name}）：用空字符串 "" 作为 key 插入 children。
//   - 字面量段（如 "api"）：用段的字面值作为 key 插入 children。
//
// segs 为空时，当前节点即为叶节点，调用 set 存储 pattern 和 handler。
func (n *routingNode) addSegments(segs []segment, p *pattern, h Handler) {
	if len(segs) == 0 {
		// 所有段已处理完毕，当前节点作为叶节点存储路由信息
		n.set(p, h)
		return
	}
	seg := segs[0]
	if seg.multi {
		// 多段通配符只能出现在最后一段（如 /files/{rest...}）
		if len(segs) != 1 {
			panic("multi wildcard not last")
		}
		c := &routingNode{}
		n.multiChild = c
		c.set(p, h)
	} else if seg.wild {
		// 单段通配符用空字符串 "" 作为子节点 key（与 emptyChild 对应）
		n.addChild("").addSegments(segs[1:], p, h)
	} else {
		// 字面量段直接以段值为 key 建立子节点
		n.addChild(seg.s).addSegments(segs[1:], p, h)
	}
}

// set sets the pattern and handler for n, which
// must be a leaf node.
//
// set 将 pattern 和 handler 写入叶节点 n。
// 若节点已有值则 panic，防止同一路径被重复注册。
func (n *routingNode) set(p *pattern, h Handler) {
	if n.pattern != nil || n.handler != nil {
		panic("non-nil leaf fields")
	}
	n.pattern = p
	n.handler = h
}

// addChild adds a child node with the given key to n
// if one does not exist, and returns the child.
//
// addChild 在节点 n 下查找或创建 key 对应的子节点并返回。
//
//   - key == "" 时走 emptyChild 快速路径（单段通配符专用优化）。
//   - 否则先在 children 中查找，不存在则新建并追加。
func (n *routingNode) addChild(key string) *routingNode {
	if key == "" {
		// 空 key 对应单段通配符，使用专用字段以避免 map 查找开销
		if n.emptyChild == nil {
			n.emptyChild = &routingNode{}
		}
		return n.emptyChild
	}
	if c := n.findChild(key); c != nil {
		return c
	}
	c := &routingNode{}
	n.children.add(key, c)
	return c
}

// findChild returns the child of n with the given key, or nil
// if there is no child with that key.
//
// findChild 返回节点 n 中 key 对应的子节点；不存在则返回 nil。
// 空 key 直接返回 emptyChild，非空 key 在 children 中查找。
func (n *routingNode) findChild(key string) *routingNode {
	if key == "" {
		return n.emptyChild
	}
	r, _ := n.children.find(key)
	return r
}

// match returns the leaf node under root that matches the arguments, and a list
// of values for pattern wildcards in the order that the wildcards appear.
// For example, if the request path is "/a/b/c" and the pattern is "/{x}/b/{y}",
// then the second return value will be []string{"a", "c"}.
//
// match 从决策树根节点出发，按照 host → method → path 的顺序匹配请求，
// 返回匹配到的叶节点以及各通配符捕获值的有序列表。
//
// 匹配策略（host 维度）：
//  1. 若请求携带 host，优先尝试精确匹配该 host 的子树。
//  2. 若精确匹配失败（或请求无 host），回退到无 host 限制的通用子树（emptyChild）。
func (root *routingNode) match(host, method, path string) (*routingNode, []string) {
	if host != "" {
		// There is a host. If there is a pattern that specifies that host and it
		// matches, we are done. If the pattern doesn't match, fall through to
		// try patterns with no host.
		// 优先在带 host 的子树中匹配；成功则直接返回，失败则继续尝试通用路由
		if l, m := root.findChild(host).matchMethodAndPath(method, path); l != nil {
			return l, m
		}
	}
	// 无 host 限制的通用路由兜底（emptyChild 对应注册时 host 为 "" 的 pattern）
	return root.emptyChild.matchMethodAndPath(method, path)
}

// matchMethodAndPath matches the method and path.
// Its return values are the same as [routingNode.match].
// The receiver should be a child of the root.
//
// matchMethodAndPath 在 host 层节点 n 下按 method 和 path 继续匹配。
//
// 匹配策略（method 维度，优先级从高到低）：
//  1. 精确匹配请求的 HTTP method（如 "GET"、"POST"）。
//  2. 若请求为 HEAD，则额外尝试 GET 子树（HTTP 规范：GET handler 兼容 HEAD）。
//  3. 以上均失败时，回退到无 method 限制的通用子树（emptyChild）。
func (n *routingNode) matchMethodAndPath(method, path string) (*routingNode, []string) {
	if n == nil {
		return nil, nil
	}
	if l, m := n.findChild(method).matchPath(path, nil); l != nil {
		// Exact match of method name.
		// 精确匹配 method 成功
		return l, m
	}
	if method == "HEAD" {
		// GET matches HEAD too.
		// HEAD 请求可以复用 GET handler
		if l, m := n.findChild("GET").matchPath(path, nil); l != nil {
			return l, m
		}
	}
	// No exact match; try patterns with no method.
	// method 均未匹配，回退到不限制 method 的通用路由
	return n.emptyChild.matchPath(path, nil)
}

// matchPath matches a path.
// Its return values are the same as [routingNode.match].
// matchPath calls itself recursively. The matches argument holds the wildcard matches
// found so far.
//
// matchPath 从节点 n 开始，按路径段递归匹配 path，累积通配符捕获值到 matches。
//
// 每个段的匹配优先级（"更具体的优先"）：
//  1. 字面量精确匹配（findChild(seg)）—— 最具体。
//  2. 单段通配符匹配（emptyChild，跳过尾部斜线 "/"）—— 次之。
//  3. 多段通配符匹配（multiChild，消费剩余全部路径）—— 最宽泛。
//
// 若某优先级匹配失败，自动回溯到下一优先级尝试（即回溯式 DFS）。
func (n *routingNode) matchPath(path string, matches []string) (*routingNode, []string) {
	if n == nil {
		return nil, nil
	}
	// If path is empty, then we are done.
	// If n is a leaf node, we found a match; return it.
	// If n is an interior node (which means it has a nil pattern),
	// then we failed to match.
	//
	// path 已消费完毕：
	//   - 叶节点（pattern != nil）→ 匹配成功。
	//   - 内部节点（pattern == nil）→ 路径段数不足，匹配失败。
	if path == "" {
		if n.pattern == nil {
			return nil, nil
		}
		return n, matches
	}
	// Get the first segment of path.
	// 切分出当前路径的第一段及剩余部分
	seg, rest := firstSegment(path)
	// First try matching against patterns that have a literal for this position.
	// We know by construction that such patterns are more specific than those
	// with a wildcard at this position (they are either more specific, equivalent,
	// or overlap, and we ruled out the first two when the patterns were registered).
	//
	// 优先尝试字面量精确匹配（注册时已保证字面量比通配符更具体）
	if n, m := n.findChild(seg).matchPath(rest, matches); n != nil {
		return n, m
	}
	// If matching a literal fails, try again with patterns that have a single
	// wildcard (represented by an empty string in the child mapping).
	// Again, by construction, patterns with a single wildcard must be more specific than
	// those with a multi wildcard.
	// We skip this step if the segment is a trailing slash, because single wildcards
	// don't match trailing slashes.
	//
	// 字面量匹配失败后，改用含单段通配符的 pattern 重新尝试
	//（单段通配符在 children 映射中以空字符串表示）。
	// 同样地，由构造过程可知，单段通配符 pattern 的具体程度一定高于多段通配符 pattern。
	// 若当前段是尾部斜线，则跳过此步骤，因为单段通配符不匹配尾部斜线。
	//
	// 字面量匹配失败，尝试单段通配符（"/" 尾部斜线不能被单段通配符消费）
	if seg != "/" {
		if n, m := n.emptyChild.matchPath(rest, append(matches, seg)); n != nil {
			return n, m
		}
	}
	// Lastly, match the pattern (there can be at most one) that has a multi
	// wildcard in this position to the rest of the path.
	//
	// 最后尝试多段通配符：将当前位置起的完整剩余路径作为一个捕获值。
	// 多段通配符最多只有一个（由注册时的冲突检测保证）。
	if c := n.multiChild; c != nil {
		// Don't record a match for a nameless wildcard (which arises from a
		// trailing slash in the pattern).
		// 匿名多段通配符（来自 pattern 尾部的 "/"）不记录捕获值
		if c.pattern.lastSegment().s != "" {
			matches = append(matches, pathUnescape(path[1:])) // remove initial slash
		}
		return c, matches
	}
	return nil, nil
}

// firstSegment splits path into its first segment, and the rest.
// The path must begin with "/".
// If path consists of only a slash, firstSegment returns ("/", "").
// The segment is returned unescaped, if possible.
//
// firstSegment 将以 "/" 开头的 path 切分为第一段和剩余部分。
//
// 返回规则：
//   - path == "/"：返回 ("/", "")，表示尾部斜线段。
//   - path == "/foo/bar"：返回 ("foo", "/bar")。
//   - path == "/foo"：返回 ("foo", "")。
//
// 返回的段已做 URL 反转义（percent-decoding），方便后续与字面量直接比较。
func firstSegment(path string) (seg, rest string) {
	if path == "/" {
		return "/", ""
	}
	path = path[1:] // drop initial slash; 去掉前导斜线，只处理段内容
	i := strings.IndexByte(path, '/')
	if i < 0 {
		// 路径中无更多斜线，整段为最后一个段
		i = len(path)
	}
	return pathUnescape(path[:i]), path[i:]
}

// matchingMethods adds to methodSet all the methods that would result in a
// match if passed to routingNode.match with the given host and path.
//
// matchingMethods 收集对给定 host + path 能够匹配成功的所有 HTTP method，
// 写入 methodSet（用于生成 405 响应的 Allow 头）。
//
// 逻辑：
//  1. 若有 host，先在 host 精确子树中收集可用 method。
//  2. 再在无 host 限制的通用子树中收集。
//  3. 若 GET 可用，则 HEAD 也自动可用（HTTP 规范）。
func (root *routingNode) matchingMethods(host, path string, methodSet map[string]bool) {
	if host != "" {
		root.findChild(host).matchingMethodsPath(path, methodSet)
	}
	root.emptyChild.matchingMethodsPath(path, methodSet)
	if methodSet["GET"] {
		methodSet["HEAD"] = true
	}
}

// matchingMethodsPath 遍历节点 n 的所有具名 method 子节点，
// 对每个 method 尝试路径匹配，成功则将该 method 加入 set。
//
// 注意：不遍历 emptyChild（无 method 限制的子树），因为该子树匹配任意 method，
// 而本函数仅在 method 匹配失败时被调用，目的是找出哪些 method 能成功匹配。
func (n *routingNode) matchingMethodsPath(path string, set map[string]bool) {
	if n == nil {
		return
	}
	n.children.eachPair(func(method string, c *routingNode) bool {
		if p, _ := c.matchPath(path, nil); p != nil {
			set[method] = true
		}
		return true
	})
	// Don't look at the empty child. If there were an empty
	// child, it would match on any method, but we only
	// call this when we fail to match on a method.
	// 不遍历 emptyChild。若存在 emptyChild，它会匹配任意 method，
	// 但本函数只在 method 匹配失败时才会被调用。
}
