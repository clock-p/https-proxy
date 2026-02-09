package proto

const (
	// HTTP 请求 -> Agent
	TReqStart   byte = 0x01
	TReqData    byte = 0x02
	TReqEnd     byte = 0x03
	TReqTrailer byte = 0x04

	// HTTP 响应 -> Gateway
	TResStart   byte = 0x11
	TResData    byte = 0x12
	TResEnd     byte = 0x13
	TResTrailer byte = 0x14

	// WebSocket（Client <-> App）消息转发
	TWsOpenOK  byte = 0x20
	TWsOpenErr byte = 0x21
	TWsC2A     byte = 0x22
	TWsA2C     byte = 0x23
	TWsClose   byte = 0x24

	// 连接重置（任一侧）
	TRst byte = 0x30
)
