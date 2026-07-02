# ksp-go2rtc — fork go2rtc cho iNut CameraLive

Fork của [AlexxIT/go2rtc](https://github.com/AlexxIT/go2rtc) v1.9.14, mục tiêu chính:
**HLS fMP4 HEVC (H.265) phát được trên iOS Safari native** cho camera Dahua/Hikvision/Tiandy,
giữ độ trễ thấp (segment 2.4s, stream copy, không transcode).

## Build

```bash
GO=/home/ksp/inut-rk3528-browswer/wpebuild/godl/go/bin/go   # Go toolchain trên máy này
CGO_ENABLED=0 $GO build -tags inut.jsc -trimpath -ldflags "-s -w" -o go2rtc_linux_amd64 .
# Cross-compile: GOOS=linux GOARCH=arm64 | GOARCH=arm GOARM=7 | GOOS=windows | GOOS=darwin
```

- Tag `inut.jsc` bật các tùy biến riêng của iNut.
- Test server: `./go2rtc_test -config go2rtc-test.yaml` (port 1984, public qua frp
  `https://go2rtc-test.a.inut.vn`). Chạy nohup, CHƯA có systemd — reboot là mất.
- Binaries host tải về qua `python3 -m http.server 8765` → `https://inut-vip-pro-dl.a.inut.vn/`.

## Config quan trọng

```yaml
hls:
  duration: 2.4   # chỉ còn là fallback lúc khởi động + EXTINF cho TS. KHÔNG cần khớp GOP nữa.
```

fMP4 cắt segment theo IDR (OnKeyframe) nên độ dài segment thật = GOP camera, không phải
`duration`. Từ 2026-07-02 (fix cam3_2nd) `Playlist()` ĐO độ dài segment thật bằng wall-clock
giữa 2 lần flush IDR và ghi EXTINF/TARGETDURATION đúng theo đó — nên camera GOP bất kỳ
(Dahua 2.4s, Xiaomi ~3.3s) đều lên lịch fetch chuẩn, không còn giật. `duration` chỉ dùng làm
ước lượng cho segment pending lúc mới start + EXTINF cho nhánh TS (TS flush theo timer nên khớp).
Trước đây EXTINF cố định = `duration`: cam GOP dài hơn (Xiaomi cam3_2nd ~3.3s vs khai 2.4s)
→ iOS lên lịch sai cadence → **giật/lag mỗi GOP**.

## Kiến thức xương máu: HLS HEVC trên iOS (chuỗi 6 bug, 2026-07-02)

Đã XÁC NHẬN chạy trên iPhone sau 6 fix (commits `b4d3a31` → `1b69578`).
Triệu chứng ban đầu: WebRTC chạy, HLS không — rồi từng lớp: không load được →
audio-only → video đen. Mỗi fix bóc một lớp:

1. **`http.Flusher` trap** (`internal/hls/session.go`): method tên `Flush()` trên Session
   vô tình thỏa interface `http.Flusher` mà `core.WriteBuffer.Write` assert sau MỖI Write
   → mỗi sample thành 1 segment (~40/s), playlist trượt nhanh hơn player poll kịp.
   → KHÔNG BAO GIỜ đặt tên method `Flush()` cho type được dùng làm `io.Writer` trong go2rtc.

2. **Segment addressing** (`internal/hls/session.go`): `Segment()` cũ pop channel, bỏ qua
   query `n`. iOS Safari/hls.js fetch lại + fetch song song → nhận sai segment → timestamp
   loạn. Fix: ring buffer 6 segment theo seq tuyệt đối, `Segment(n)` long-poll 30s.

3. **1 moof/sample** (`internal/hls/fmp4.go` — `repackFMP4`): muxer go2rtc sinh 1 moof+mdat
   mỗi sample (kiểu MSE). Segment chứa ~100 fragment 1-frame: Chrome nuốt, AVFoundation
   TỪ CHỐI. Fix: gộp thành 1 moof (1 traf/track, **traf video TRƯỚC audio** — thứ tự theo
   trackID; xếp theo thứ tự xuất hiện sẽ ra audio trước vì audio đến ngay sau nhát cắt IDR)
   + 1 mdat, trun kiểu ffmpeg (tfhd 0x020038 defaults, trun 0x205 video / 0x201 audio).

4. **Conformance window** (`pkg/h265/sps.go`): SPS parser thiếu crop — khai 2880×1624
   (coded) thay vì 2880×1620 (display). ISO 14496-15 bắt buộc kích thước ĐÃ CROP trong
   sample entry; VideoToolbox từ chối track khi lệch (audio-only).

5. **hvcC numTemporalLayers** (`pkg/h265/mpeg4.go`): byte 21 của hvcC từng chỉ ghi
   lengthSize → numTemporalLayers=0 ("unknown"). Apple/ffmpeg luôn ghi ≥1. Đọc từ SPS
   (thường ra 0x0F).

6. **GỐC RỄ video đen — Dahua glued slices** (`pkg/h265/helper.go` — `SplitEmbeddedAnnexB`):
   firmware Dahua nhét NHIỀU slice của một frame vào một luồng RTP FU, ngăn cách bằng
   start code Annex-B NHÚNG BÊN TRONG. Depay ngây thơ dán thành 1 NAL khổng lồ hỏng
   (slice1 + `00000001` + slice2). ffmpeg/WebRTC decoder tự resync trên start code nên
   vẫn chạy; iOS VideoToolbox từ chối sample → **đen hình, còn tiếng**. Fix: tách theo
   `00 00 01` trong `h265.RTPDepay` (an toàn vì emulation prevention đảm bảo chuỗi này
   không thể xuất hiện trong NAL hợp lệ).

Ngoài ra (từ các commit trước): hvc1 (KHÔNG dùng hev1 — Apple cấm), param sets phải nằm
trong hvcC với array_completeness=1 và bị strip khỏi mdat (`StripParamSets`); level_idc
phải điền đúng kẻo iOS rơi về software decode; audio AAC < 16kHz bị iOS mới từ chối.

## Kỹ thuật debug đã chứng minh hiệu quả

- **A/B cùng camera**: `ffmpeg -rtsp_transport tcp -i <rtsp> -c copy -tag:v hvc1 -f hls
  -hls_segment_type fmp4 -hls_time 2.4 -hls_playlist_type vod out.m3u8` làm mẫu đối chứng
  "chắc chắn đúng" — nếu ffmpeg chạy mà mình không: diff từng box là ra.
- **VOD tĩnh để cô lập**: host init + vài segment + m3u8 VOD tĩnh (thư mục `iostest/`)
  → tách bạch lỗi playlist-live vs lỗi media.
- **Chuẩn vàng**: stream bipbop HEVC chính chủ Apple
  (`devstreaming-cdn.apple.com/videos/streaming/examples/bipbop_adv_example_hevc/`).
- **Đọc triệu chứng iOS**: không load = playlist/giao thức; audio-only = init/track-level
  (tkhd/hvcC/dims); đen hình còn tiếng = nội dung sample (bitstream/slice).
- Soi box MP4 nhanh bằng script python (parse ftyp/moof/tfhd/tfdt/trun/mdat + NAL walker).
- `runtime.Callers` println trong `flush()` tìm ra bug #1 khi suy luận tĩnh bó tay.

## Camera test (go2rtc-test.yaml)

`dahua_upgrade` = Dahua HEVC 2880×1620@25, GOP 60, AAC 16kHz mono, multi-slice IDR
(chính là camera lộ bug #6). Các cam khác: hikvision_smart/analog, tiandy, imou, cam3.
