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
	// loaded == false：当前协程是 leader，执行 fn
	// loaded == true：说明已有 leader 在跑，当前协程 Wait()
	actual, loaded := g.m.LoadOrStore(key, newCall)//LoadOrStore 如果key存在，则返回value，否则存储新的value
	c := actual.(*call)
	if loaded {
		c.wg.Wait() // 等待正在进行的请求完成  newCall废弃
		return c.val, c.err
	}

	// 当前协程成为 leader，负责执行 fn
	defer g.m.Delete(key)
	defer c.wg.Done()

	c.val, c.err = fn()
	return c.val, c.err
}
