package singleflight

import (
	"sync"
)

// 代表正在进行或已结束的请求
type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// Group manages all kinds of calls
type Group struct {
	m sync.Map // 使用sync.Map来优化并发性能
}

// Do 针对相同的key，保证多次调用Do()，都只会调用一次fn
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	newCall := &call{}
	newCall.wg.Add(1)

	actual, loaded := g.m.LoadOrStore(key, newCall)
	c := actual.(*call)

	if loaded {
		c.wg.Wait() // Wait for the existing request to finish
		return c.val, c.err
	}

	// 当前协程成为 leader，负责执行 fn
	defer g.m.Delete(key)
	defer c.wg.Done()

	c.val, c.err = fn()
	return c.val, c.err
}
