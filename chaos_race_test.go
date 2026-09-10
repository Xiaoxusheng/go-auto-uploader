package main

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"upload/internal/config"
)

// TestConcurrentStateMutations 启动混沌测试引擎。
// 该函数在同一时间内开启上百个 Goroutine 对系统的核心配置表、状态字典和日志流进行疯狂的增删改查。
// 其目的不是验证业务结果，而是配合 Go 编译器的 -race 标志，强制暴露底层是否存在读写锁死或并发越界访问。
func TestConcurrentStateMutations(t *testing.T) {
	var wg sync.WaitGroup
	routineCount := 50          // 读写各 50 个高频协程
	duration := 2 * time.Second // 持续轰炸 2 秒

	// 模拟写入端：疯狂改写状态和推送日志
	for i := 0; i < routineCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			timeout := time.After(duration)
			counter := 0
			for {
				select {
				case <-timeout:
					return
				default:
					// 1. 疯狂写入或更新内置引擎状态 Map
					updateBuiltinStatus(
						"TestPlatform",
						"Room_"+strconv.Itoa(workerID),
						"Anchor_"+strconv.Itoa(counter),
						"",
						"hd",
						"录制中",
					)

					// 2. 疯狂推送并发日志
					appLogs.Add("INFO", "Chaos Test", "")

					// 3. 疯狂更替热重载配置
					cfgStore.Update(func(c *config.Config) { c.Workers = counter%10 + 1 })

					counter++
				}
			}
		}(i)
	}

	// 模拟读取端：疯狂遍历和抓取状态聚合
	for i := 0; i < routineCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := time.After(duration)
			for {
				select {
				case <-timeout:
					return
				default:
					// 1. 疯狂遍历读取状态表
					_ = GetBuiltinRecorderTasks()

					// 2. 疯狂读取全局配置
					_ = appCfg().Workers

					// 3. 疯狂调用构建数据宽表 (内部包含大量锁获取操作)
					_ = buildStatusData()

					// 极短休眠让出 CPU 时间片，使得读写交替更激烈
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
	t.Log("混沌读写压力测试结束，未发生死锁。请确保使用了 -race 标志确认无数据竞争。")
}
