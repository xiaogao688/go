// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

// mapping 是一个键唯一的键值对集合（泛型实现）。
// 零值的 mapping 为空，可直接使用，无需初始化。
//
// 核心设计思路——自适应底层存储：
//   - 元素较少时（≤ maxSlice 个），使用切片 s 存储，利用 CPU 缓存局部性，
//     线性遍历往往比哈希计算更快。
//   - 元素较多时，自动迁移到 map m 存储，查找复杂度降为 O(1)。
//
// 这种策略使 [mapping.find] 在不同数据规模下都能保持最优效率。
type mapping[K comparable, V any] struct {
	s []entry[K, V] // 少量键值对时使用切片存储（顺序线性查找）
	m map[K]V       // 大量键值对时使用哈希表存储（O(1) 查找）
}

// entry 是 mapping 切片模式下的单个键值对节点。
type entry[K comparable, V any] struct {
	key   K
	value V
}

// maxSlice 是切片模式的最大容量阈值，超过此数量后切换为 map 存储。
// 设为变量而非常量，方便基准测试时动态调整以寻找最优临界点。
var maxSlice int = 8

// add 向 mapping 中添加一个键值对。
//
// 存储策略：
//  1. 若当前为切片模式（m == nil）且元素数未达阈值，直接追加到切片 s。
//  2. 若切片已满且尚未升级，则触发一次性迁移：
//     将 s 中所有元素复制到新建的 map，清空 s，完成从切片到 map 的切换。
//  3. 升级完成后，新键值对直接写入 map。
//
// 注意：迁移是单向的，map 模式不会降级回切片模式。
func (h *mapping[K, V]) add(k K, v V) {
	if h.m == nil && len(h.s) < maxSlice {
		// 切片未满，直接追加
		h.s = append(h.s, entry[K, V]{k, v})
	} else {
		if h.m == nil {
			// 切片已满，首次触发升级：将切片数据迁移到 map
			h.m = map[K]V{}
			for _, e := range h.s {
				h.m[e.key] = e.value
			}
			h.s = nil // 释放切片，避免内存冗余
		}
		h.m[k] = v
	}
}

// find 根据键查找对应的值。
// 第二个返回值 found 表示键是否存在：存在为 true，不存在为 false。
//
// 查找策略与当前存储模式对应：
//   - map 模式：直接哈希寻址，O(1)。
//   - 切片模式：线性遍历，O(n)，但 n ≤ maxSlice，常数极小且缓存友好。
//   - h == nil：安全短路，避免空指针 panic。
func (h *mapping[K, V]) find(k K) (v V, found bool) {
	if h == nil {
		return v, false
	}
	if h.m != nil {
		v, found = h.m[k]
		return v, found
	}
	for _, e := range h.s {
		if e.key == k {
			return e.value, true
		}
	}
	return v, false
}

// eachPair 遍历 mapping 中的所有键值对，对每一对调用回调函数 f。
// 若 f 返回 false，则立即中断遍历（支持提前退出）。
//
// 遍历顺序：
//   - map 模式：顺序不确定（Go map 随机迭代）。
//   - 切片模式：按插入顺序遍历。
func (h *mapping[K, V]) eachPair(f func(k K, v V) bool) {
	if h == nil {
		return
	}
	if h.m != nil {
		for k, v := range h.m {
			if !f(k, v) {
				return
			}
		}
	} else {
		for _, e := range h.s {
			if !f(e.key, e.value) {
				return
			}
		}
	}
}
