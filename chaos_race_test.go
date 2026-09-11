package main

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"upload/internal/app"
	"upload/internal/config"
	"upload/internal/recorder"
)

func TestConcurrentStateMutations(t *testing.T) {
	var wg sync.WaitGroup
	routineCount := 50
	duration := 2 * time.Second

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
					recorder.UpdateStatus(
						"TestPlatform",
						"Room_"+strconv.Itoa(workerID),
						"Anchor_"+strconv.Itoa(counter),
						"",
						"hd",
						"录制中",
					)
					app.AppLogs.Add("INFO", "Chaos Test", "")
					app.CfgStore.Update(func(c *config.Config) { c.Workers = counter%10 + 1 })
					counter++
				}
			}
		}(i)
	}

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
					_ = recorder.Tasks()
					_ = app.AppCfg().Workers
					_ = buildStatusData()
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
	t.Log("混沌读写压力测试结束，未发生死锁。请确保使用了 -race 标志确认无数据竞争。")
}
