// Copyright (C) 2012 Numerotron Inc.
// Use of this source code is governed by an MIT-style license
// that can be found in the LICENSE file.

// Package consistent provides a consistent hashing function.
//
// Consistent hashing is often used to distribute requests to a changing set of servers.  For example,
// say you have some cache servers cacheA, cacheB, and cacheC.  You want to decide which cache server
// to use to look up information on a user.
//
// You could use a typical hash table and hash the user id
// to one of cacheA, cacheB, or cacheC.  But with a typical hash table, if you add or remove a server,
// almost all keys will get remapped to different results, which basically could bring your service
// to a grinding halt while the caches get rebuilt.
//
// With a consistent hash, adding or removing a server drastically reduces the number of keys that
// get remapped.
//
// Read more about consistent hashing on wikipedia:  http://en.wikipedia.org/wiki/Consistent_hashing
//
package consistent // import "stathat.com/c/consistent"

import (
	"errors"
	"hash/crc32"
	"sort"
	"strconv"
	"sync"
)

// 支持排序的uint32分片
type uints []uint32

// Len returns the length of the uints array.
func (x uints) Len() int { return len(x) }

// Less returns true if element i is less than element j.
func (x uints) Less(i, j int) bool { return x[i] < x[j] }

// Swap exchanges elements i and j.
func (x uints) Swap(i, j int) { x[i], x[j] = x[j], x[i] }

// ErrEmptyCircle is the error returned when trying to get an element when nothing has been added to hash.
var ErrEmptyCircle = errors.New("empty circle")

// Consistent holds the information about the members of the consistent hash circle.
type Consistent struct {
	circle           map[uint32]string //记录虚拟节点hash和物理节点key的映射关系
	members          map[string]bool   //记录添加到哈希环的物理节点key
	sortedHashes     uints             //保存排序后的虚拟节点hash(uint32类型),方便进行二分查找
	NumberOfReplicas int               //每个节点虚拟节点副本数量(包含物理节点)
	count            int64             //物理节点数量
	scratch          [64]byte          //此字段未使用
	sync.RWMutex
}

// New creates a new Consistent object with a default setting of 20 replicas for each entry.
//
// To change the number of replicas, set NumberOfReplicas before adding entries.
func New() *Consistent {
	c := new(Consistent)
	c.NumberOfReplicas = 20
	c.circle = make(map[uint32]string)
	c.members = make(map[string]bool)
	return c
}

// 根据key和NumberOfReplicas的索引生成虚拟节点key
// eltKey generates a string key for an element with an index.
func (c *Consistent) eltKey(elt string, idx int) string {
	// return elt + "|" + strconv.Itoa(idx)
	return strconv.Itoa(idx) + elt
}

// 添加节点到哈希环 加锁
// Add inserts a string element in the consistent hash.
func (c *Consistent) Add(elt string) {
	c.Lock()
	defer c.Unlock()
	c.add(elt)
}

// 增加物理节点 无锁
// need c.Lock() before calling
func (c *Consistent) add(elt string) {
	for i := 0; i < c.NumberOfReplicas; i++ {
		c.circle[c.hashKey(c.eltKey(elt, i))] = elt
	}
	c.members[elt] = true
	c.updateSortedHashes()
	c.count++
}

// 删除物理节点 加锁
// Remove removes an element from the hash.
func (c *Consistent) Remove(elt string) {
	c.Lock()
	defer c.Unlock()
	c.remove(elt)
}

// 删除物理节点 无锁
// need c.Lock() before calling
func (c *Consistent) remove(elt string) {
	for i := 0; i < c.NumberOfReplicas; i++ {
		delete(c.circle, c.hashKey(c.eltKey(elt, i)))
	}
	delete(c.members, elt)
	c.updateSortedHashes()
	c.count--
}

// 对比member和elts,不存在在elts中的元素清除,elts多出的元素加入member
// Set sets all the elements in the hash.  If there are existing elements not
// present in elts, they will be removed.
func (c *Consistent) Set(elts []string) {
	c.Lock()
	defer c.Unlock()
	for k := range c.members {
		found := false
		for _, v := range elts {
			if k == v {
				found = true
				break
			}
		}
		if !found {
			c.remove(k)
		}
	}
	for _, v := range elts {
		_, exists := c.members[v]
		if exists {
			continue
		}
		c.add(v)
	}
}

// 返回所有物理节点
func (c *Consistent) Members() []string {
	c.RLock()
	defer c.RUnlock()
	var m []string
	for k := range c.members {
		m = append(m, k)
	}
	return m
}

// 获取元素(应该)所在的物理节点
// Get returns an element close to where name hashes to in the circle.
func (c *Consistent) Get(name string) (string, error) {
	c.RLock()
	defer c.RUnlock()
	if len(c.circle) == 0 {
		return "", ErrEmptyCircle
	}
	// 求对象hash
	key := c.hashKey(name)
	// 查找hash对应虚拟节点位置
	i := c.search(key)
	// 查找虚拟节点对应的物理节点
	return c.circle[c.sortedHashes[i]], nil
}

// 返回key所属虚拟节点hash
func (c *Consistent) search(key uint32) (i int) {
	// 闭包函数
	f := func(x int) bool {
		return c.sortedHashes[x] > key
	}
	// 二分查找,返回符合f的最小索引
	i = sort.Search(len(c.sortedHashes), f)
	if i >= len(c.sortedHashes) {
		i = 0
	}
	return
}

// 获取最邻近的两个物理节点
// GetTwo returns the two closest distinct elements to the name input in the circle.
func (c *Consistent) GetTwo(name string) (string, string, error) {
	c.RLock()
	defer c.RUnlock()
	if len(c.circle) == 0 {
		return "", "", ErrEmptyCircle
	}
	key := c.hashKey(name)
	i := c.search(key)
	a := c.circle[c.sortedHashes[i]]

	if c.count == 1 {
		return a, "", nil
	}

	start := i
	var b string
	for i = start + 1; i != start; i++ {
		if i >= len(c.sortedHashes) {
			i = 0
		}
		b = c.circle[c.sortedHashes[i]]
		if b != a {
			break
		}
	}
	return a, b, nil
}

// 获取最邻近的N个物理节点
// GetN returns the N closest distinct elements to the name input in the circle.
func (c *Consistent) GetN(name string, n int) ([]string, error) {
	c.RLock()
	defer c.RUnlock()

	if len(c.circle) == 0 {
		return nil, ErrEmptyCircle
	}

	if c.count < int64(n) {
		n = int(c.count)
	}

	var (
		key   = c.hashKey(name)
		i     = c.search(key)
		start = i
		res   = make([]string, 0, n)
		elem  = c.circle[c.sortedHashes[i]]
	)

	res = append(res, elem)

	if len(res) == n {
		return res, nil
	}

	for i = start + 1; i != start; i++ {
		if i >= len(c.sortedHashes) {
			i = 0
		}
		elem = c.circle[c.sortedHashes[i]]
		if !sliceContainsMember(res, elem) {
			res = append(res, elem)
		}
		if len(res) == n {
			break
		}
	}

	return res, nil
}

// 返回hash值,类型为uint32
func (c *Consistent) hashKey(key string) uint32 {
	if len(key) < 64 {
		var scratch [64]byte
		copy(scratch[:], key)
		return crc32.ChecksumIEEE(scratch[:len(key)])
	}
	return crc32.ChecksumIEEE([]byte(key))
}

// 将虚拟节点hash,进行排序保存
func (c *Consistent) updateSortedHashes() {
	hashes := c.sortedHashes[:0]
	// len(c.circle)总虚拟节点数
	//
	//reallocate if we're holding on to too much (1/4th)
	if cap(c.sortedHashes)/(c.NumberOfReplicas*4) > len(c.circle) {
		hashes = nil
	}
	// 遍历circle的key(虚拟节点hash值),放入排序分片,方便二分查找
	for k := range c.circle {
		hashes = append(hashes, k)
	}
	sort.Sort(hashes)
	c.sortedHashes = hashes
}

// member是否在set中
func sliceContainsMember(set []string, member string) bool {
	for _, m := range set {
		if m == member {
			return true
		}
	}
	return false
}
