package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"buddy2api-go/internal/upstream"
)

// TestLockOrderInversion 锁顺序回归：Manager.mu 与 Router.mu 必须单向嵌套。
//
// 历史 bug（本测试就是它的回归用例）：Snapshot 持 m.mu.RLock 后调 router.Count()
// （要 router.mu.RLock），而 Router.Resolve 慢路径持 router.mu.Lock 后回调
// m.usable()（要 m.mu.RLock）—— 一旦有一个 m.mu 写者（NoteError）在等，
// m.mu 进入「有写者等待」状态，后续 RLock 被挡住 → 三方 ABBA 死锁，整个网关卡死。
//
// 修法：锁顺序强制单向 Manager → Router，Router 内部绝不回调 Manager；
// Snapshot 的 Router 取数全部移到 m.mu 之外。
func TestLockOrderInversion(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2", "acc3", "acc4")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(3)
	// A: 反复取快照（持 m.mu.RLock）
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.Snapshot(nil)
		}
	}()
	// B: 反复用新会话触发 Router 慢路径（持 router.mu.Lock）
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			m.Resolve(fmt.Sprintf("sess-%d", i), "glm", map[string]bool{})
		}
	}()
	// C: 反复写 Manager.mu（制造「写者等待」窗口）
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.NoteError("acc1", "glm", upstream.ErrKindServer, []byte(`{}`))
		}
	}()

	go func() { wg.Wait(); close(done) }()

	// 让三轮循环跑一会儿，然后把「是否能干净退出」当作存活判据：
	// 死锁时 goroutine 卡在锁上，收到 stop 也不会退出。
	time.Sleep(2 * time.Second)
	close(stop)
	select {
	case <-done:
		// 正常：三个 goroutine 都退出了
	case <-time.After(10 * time.Second):
		t.Fatal("死锁：关闭 stop 后 10 秒内 goroutine 仍未退出 —— " +
			"Manager.mu 与 Router.mu 锁顺序反转（Manager→Router 必须单向）")
	}
}
