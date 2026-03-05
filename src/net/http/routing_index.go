// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import "math"

// A routingIndex optimizes conflict detection by indexing patterns.
//
// The basic idea is to rule out patterns that cannot conflict with a given
// pattern because they have a different literal in a corresponding segment.
// See the comments in [routingIndex.possiblyConflictingPatterns] for more details.
type routingIndex struct {
	// map from a particular segment position and value to all registered patterns
	// with that value in that position.
	// For example, the key {1, "b"} would hold the patterns "/a/b" and "/a/b/c"
	// but not "/a", "b/a", "/a/c" or "/a/{x}".
	segments map[routingIndexKey][]*pattern
	// All patterns that end in a multi wildcard (including trailing slash).
	// We do not try to be clever about indexing multi patterns, because there
	// are unlikely to be many of them.
	multis []*pattern
}

type routingIndexKey struct {
	pos int    // 0-based segment position
	s   string // literal, or empty for wildcard
}

func (idx *routingIndex) addPattern(pat *pattern) {
	if pat.lastSegment().multi {
		idx.multis = append(idx.multis, pat)
	} else {
		if idx.segments == nil {
			idx.segments = map[routingIndexKey][]*pattern{}
		}
		for pos, seg := range pat.segments {
			key := routingIndexKey{pos: pos, s: ""}
			if !seg.wild {
				key.s = seg.s
			}
			idx.segments[key] = append(idx.segments[key], pat)
		}
	}
}

// possiblyConflictingPatterns 对所有可能与 pat 冲突的已注册 pattern 调用 f。
// 若 f 返回非 nil 错误，则立即返回该错误。
//
// 为保证正确性，此函数必须覆盖所有真正可能冲突的 pattern，但允许包含不会冲突的 pattern（误报）。
// 极端情况下，返回全部已注册 pattern 也是正确的。
// 我们利用这一宽松要求来简化实现，通过索引剪枝减少候选集，而不追求精确。
func (idx *routingIndex) possiblyConflictingPatterns(pat *pattern, f func(*pattern) error) (err error) {
	// 三种 pattern 术语：
	//   dollar pattern:   以 "{$}" 结尾，如 /a/b/{$}，只匹配精确路径末尾的斜杠
	//   multi pattern:    以尾斜杠或 "{x...}" 结尾，可匹配任意多段路径
	//   ordinary pattern: 普通固定长度 pattern，两者皆非

	// apply 对列表中每个 pattern 调用 f，遇到错误立即停止。
	// 通过闭包共享外层具名返回值 err，使多次 apply 调用能感知之前是否已出错。
	apply := func(pats []*pattern) error {
		if err != nil {
			return err // 已有错误，直接跳过
		}
		for _, p := range pats {
			err = f(p)
			if err != nil {
				return err
			}
		}
		return nil
	}

	// ① multi pattern 无法通过索引剪枝（它们可以匹配任意路径长度），
	// 因此对所有已注册的 multi pattern 都调用 f。
	if err := apply(idx.multis); err != nil {
		return err
	}
	if pat.lastSegment().s == "/" {
		// ② dollar pattern 分支：
		// dollar pattern 只匹配以 "/" 结尾的路径，ordinary pattern 不会。
		// 因此 dollar pattern 只会与同位置的其他 dollar pattern（或 multi）冲突。
		// 用 (pos = 最后一个段, s = "/") 作为索引键直接查找，无需检查其他 pattern。
		return apply(idx.segments[routingIndexKey{s: "/", pos: len(pat.segments) - 1}])
	}
	// ③ ordinary / multi pattern 分支：
	// 冲突只可能发生在某个字面量位置，另一 pattern 在同位置有相同字面量或通配符。
	// 优化策略：不对每个位置的候选集求交集（复杂），而是找候选数量最少的那个位置，
	// 只检查该位置的候选集。允许误报，但不能漏报真正冲突的 pattern。
	var lmin, wmin []*pattern // 候选最少位置的：字面量匹配列表、通配符匹配列表
	min := math.MaxInt
	hasLit := false // pat 中是否存在字面量段
	for i, seg := range pat.segments {
		if seg.multi {
			break // multi 段之后不再有固定段，停止扫描
		}
		if !seg.wild { // 只有字面量段才能用于剪枝
			hasLit = true
			lpats := idx.segments[routingIndexKey{s: seg.s, pos: i}] // 同位置、同字面量
			wpats := idx.segments[routingIndexKey{s: "", pos: i}]    // 同位置、任意通配符
			if sum := len(lpats) + len(wpats); sum < min {
				// 记录候选总数最少的那个位置
				lmin = lpats
				wmin = wpats
				min = sum
			}
		}
	}
	if hasLit {
		// 用候选最少的字面量位置的两组 pattern 来检查，以减少不必要的冲突判断
		apply(lmin) // 字面量匹配的 pattern
		apply(wmin) // 通配符匹配的 pattern
		return err
	}

	// pat 全部由通配符组成（如 /{a}/{b}/{c}），无法通过任何字面量剪枝，
	// 只能对所有已注册的 pattern 调用 f。
	for _, pats := range idx.segments {
		apply(pats)
	}
	return err
}
