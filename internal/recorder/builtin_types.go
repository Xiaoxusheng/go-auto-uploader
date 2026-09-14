package recorder

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"upload/internal/config"
)

var builtinFfmpegPath = "ffmpeg"

var builtinHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	},
}

// builtinSafeProxyClient 专用于图片代理的 SSRF 防护客户端：
// 在建连前对解析出的目标 IP 做内网/环名校验，并强制以校验后的 IP 直连，杜绝 DNS 重绑定绕过
var builtinSafeProxyClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns: 100,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("代理目标域名无法解析: %s", host)
			}
			// 任一解析结果命中高危网段都直接拒绝，防止多记录解析混入内网地址
			for _, ip := range ips {
				if isForbiddenProxyIP(ip.IP) {
					return nil, fmt.Errorf("代理目标命中内网/保留网段，已拒绝: %s", ip.IP)
				}
			}
			// 以校验通过的 IP 直连，后续连接不会再经历二次 DNS 解析
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	},
}

// isForbiddenProxyIP 判定目标 IP 是否属于 SSRF 高危网段（环回/内网/链路本地/组播/未指定）
func isForbiddenProxyIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

type BuiltinTaskStatus struct {
	Platform   string `json:"platform"`
	RoomID     string `json:"room_id"`
	AnchorName string `json:"anchor_name"`
	Avatar     string `json:"avatar"`
	Quality    string `json:"quality"`
	Status     string `json:"status"`
	UpdateTime string `json:"update_time"`
	IsPaused   bool   `json:"is_paused"`
	FileSize   string `json:"file_size"`
	Duration   string `json:"duration"`
	Record     bool   `json:"record"`
	Screenshot bool   `json:"screenshot"`
	// ShotInterval 单主播专属截图间隔（秒）；0 表示跟随全局设置
	ShotInterval int `json:"shot_interval"`
	// Watermark 单主播水印三态：0=跟随全局，1=强制开，2=强制关
	Watermark int `json:"watermark"`
	// QualityOverride 单主播画质覆盖："" = 跟随全局（uhd/hd/sd）
	QualityOverride string `json:"quality_override"`
	// MaxDuration 单主播单场最长录制时长（分钟）；0 = 不限制
	MaxDuration int `json:"max_duration"`
	// SegmentTime 单主播专属切片时长（分钟）；0 = 跟随全局「自动分片时长」
	SegmentTime int `json:"segment_time"`
	// Window 单主播录制时段（"HH:MM-HH:MM"）；空 = 全天可录
	Window string `json:"window"`

	startTime time.Time `json:"-"`
}

// BuiltinTaskFlags 单主播「录屏 / 截屏」独立开关（实现见 internal/recorder）
type BuiltinTaskFlags = TaskFlags

func defaultBuiltinTaskFlags() BuiltinTaskFlags {
	return DefaultFlags()
}

// isBuiltinLiveStatus 判定任务是否处于“已接管推流”的活跃状态（录屏或截屏中）
func isBuiltinLiveStatus(s string) bool {
	return IsLiveStatus(s)
}

func builtinFlagsKey(platform, roomID string) string {
	return platform + "_" + roomID
}

func getBuiltinTaskFlags(platform, roomID string) BuiltinTaskFlags {
	if v, ok := builtinTaskFlags.Load(builtinFlagsKey(platform, roomID)); ok {
		if f, ok := v.(BuiltinTaskFlags); ok {
			return f
		}
	}
	return defaultBuiltinTaskFlags()
}

func setBuiltinTaskFlags(platform, roomID string, f BuiltinTaskFlags) {
	builtinTaskFlags.Store(builtinFlagsKey(platform, roomID), f)
}

var (
	// builtinCfgPtr 内置引擎配置的原子快照指针。
	// 写方一律「复制-修改-整体发布」，读方只读快照，从根本上消除
	// HTTP 处理器写入与监控协程读取之间的数据竞争（string 字段撕裂会崩）。
	builtinCfgPtr      atomic.Pointer[BuiltinConfig]
	builtinActiveTasks sync.Map
	builtinStatusMap   sync.Map
	builtinCookies     *BuiltinCookieConfig
	builtinCookieMutex sync.RWMutex

	builtinTaskStates  sync.Map // key: platform_roomID, value: "running", "paused", "deleted"
	builtinCancels     sync.Map // key: platform_roomID, value: context.CancelFunc
	builtinCustomNames sync.Map // 内存中保存的自定义名称 (由 txt 提供)
	builtinTaskFlags   sync.Map // key: platform_roomID, value: BuiltinTaskFlags

	// ✨ 添加全局防抖缓冲池：彻底消除由于网络颠簸引起的 FFmpeg 开播/下播反复横跳现象
	builtinNotifyDebounce sync.Map

	// 配置热重载标记：key=platform_roomID。
	// 因切换视频水印等「只在 ffmpeg 启动时生效」的开关而主动中断会话时打标，
	// 让监控循环知道这是配置重开、不是断流：不报下播、也不走 30 秒退避。
	builtinConfigRestart sync.Map

	// ✨ 新增：全局广播防抖信号通道
	builtinBroadcastChan = make(chan struct{}, 1)
)

var builtinAnchorLinesMutex sync.Mutex

// BuiltinConfig 内置录制引擎配置：类型定义已收敛到 internal/config（统一 config.json），
// 此处用别名保持既有调用点不变。
type BuiltinConfig = config.BuiltinSettings

// BuiltinCookieConfig 多平台鉴权会话别名。
type BuiltinCookieConfig = config.BuiltinCookies

// BuiltinPlatform 定义平台扩展必须要实现的公共规范接口
type BuiltinPlatform interface {
	GetPlatformName() string
	GetStreamURL(roomID string, quality string) (streamURL string, anchorName string, avatar string, err error)
}

// startBuiltinBroadcastDebouncer 启动全局广播防抖守护协程，限制最高刷新频率，聚合高并发状态更新
