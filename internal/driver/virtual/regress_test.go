package virtual

import (
	"context"
	"testing"

	"uic/internal/driver"
	"uic/internal/model"
)

func numFloat(v float64) model.Value { return model.NumValue(model.TypeFloat64, v) }

// 回归：fault=disconnect 时「第几次请求」的三个入口（Probe / Read / Write）
// 必须给出同一个结论。
//
// 历史缺陷：faultAt 只对 Read 生效，Probe 与 Write 无条件断连。于是
// fault=disconnect + faultAt>0（本意「运行到第 n 次才掉线」）是死配置：
// 建链时 Probe 先断，连接永远建不起来，Read 一次都不执行，更等不到恢复；
// 而 faultAt=0 时又是 Probe/Write 断、Read 正常——同一台设备的三个入口
// 结论相反。配置表把 faultAt 定义为「第几次请求注入」，三个入口都该遵守。
func TestDisconnectFaultAtIsConsistentAcrossProbeReadWrite(t *testing.T) {
	// faultAt>0：前几轮全通，第 n 轮起 Probe 也断。
	c := open(t, map[string]string{"mode": "const", "fault": "disconnect", "faultAt": "3"})
	for i := 1; i <= 2; i++ {
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("faultAt=3：第 %d 次 Probe 不该断（注入点是第 3 次），got %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		readOne(t, c, floatPoint("p1"))
	}
	if err := c.Probe(context.Background()); err == nil {
		t.Fatal("faultAt=3：第 3 次 Probe 应注入断连")
	} else if driver.KindOf(err) != driver.ErrDisconnected {
		t.Fatalf("断连错误种类 = %v，want disconnected", driver.KindOf(err))
	}

	// faultAt=0 / 负值：表示「不按次数注入」，三个入口都不断连。
	for _, at := range []string{"0", "-1"} {
		c := open(t, map[string]string{"mode": "const", "fault": "disconnect", "faultAt": at})
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("faultAt=%s：Probe 不该断，got %v", at, err)
		}
		for i := 0; i < 3; i++ {
			readOne(t, c, floatPoint("p1"))
		}
		written, err := c.Write(context.Background(), []driver.WriteRequest{
			{PointID: "p1", DataType: "float64", Value: numFloat(1.5)},
		})
		if err != nil {
			t.Fatalf("faultAt=%s：Write 不该断，got %v", at, err)
		}
		if len(written) != 1 || !written[0].OK {
			t.Fatalf("faultAt=%s：Write = %+v", at, written)
		}
	}
}

// 回归：非 disconnect 的故障（drop/timeout/bad/address）只发生在 Read 的
// 逐点注入路径上，Probe 必须始终通过——否则建链探活会失败，整轮采集根本
// 跑不起来（推送型/内存型设备的纪律：只有真正的链路级故障才上抛 Probe）。
func TestNonDisconnectFaultsKeepProbeHealthy(t *testing.T) {
	for _, fault := range []string{"drop", "timeout", "bad", "address"} {
		c := open(t, map[string]string{"mode": "const", "fault": fault})
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("fault=%s：Probe 不该被非断连故障影响，got %v", fault, err)
		}
		written, err := c.Write(context.Background(), []driver.WriteRequest{
			{PointID: "p1", DataType: "float64", Value: numFloat(2.5)},
		})
		if err != nil {
			t.Fatalf("fault=%s：Write 不该被非断连故障影响，got %v", fault, err)
		}
		if len(written) != 1 || !written[0].OK {
			t.Fatalf("fault=%s：Write = %+v", fault, written)
		}
	}
}

// 回归：fault=drop/timeout 是临时性（temporary）故障，断连也是临时性的
// ——collector 对临时错误重连/重试而不熔断。调度器靠 temporary 区分
// 「重试本次」还是「重连后再试」。
// 通过 Read 触发 inject：Read 是逐点注入路径，faultAt=1 时第一次读即命中。
func TestTimeoutAndDisconnectAreTemporary(t *testing.T) {
	for _, fault := range []string{"timeout", "disconnect"} {
		c := open(t, map[string]string{"mode": "const", "fault": fault, "faultAt": "1"})
		_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{floatPoint("p1")}}})
		if err == nil || !driver.IsRetryable(err) {
			t.Fatalf("%s 应是临时（temporary）故障，err=%v retryable=%v", fault, err, driver.IsRetryable(err))
		}
	}
	// 坏帧/地址错是配置级错误：不可重试，且不熔断。
	for _, fault := range []string{"bad", "address"} {
		c := open(t, map[string]string{"mode": "const", "fault": fault, "faultAt": "1"})
		_, err := c.Read(context.Background(), []driver.ReadSpan{{ID: "s", Points: []driver.PointRef{floatPoint("p1")}}})
		if err == nil || driver.IsRetryable(err) {
			t.Fatalf("%s 应是非临时（permanent）故障，err=%v retryable=%v", fault, err, driver.IsRetryable(err))
		}
	}
}
