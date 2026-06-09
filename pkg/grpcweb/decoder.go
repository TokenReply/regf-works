package grpcweb

import (
	"encoding/binary"
	"fmt"
)

// DecodeSessionCookie 从 CreateUserAndSessionV2 的 gRPC-web 响应中提取 session_cookie
//
// 响应结构:
//
//	gRPC-web frame: \x00 + 4字节长度 + payload
//	CreateSessionV2Response {
//	  oneof response {
//	    CreateSessionResponse session = 1; ← 成功
//	    ExistingEmailSignInMethods existing = 2; ← 邮箱已存在
//	  }
//	}
//	CreateSessionResponse {
//	  Session session = 1;
//	  string session_cookie = 2; ← SSO token
//	  repeated string one_time_link_tokens = 3;
//	}
func DecodeSessionCookie(body []byte) (string, error) {
	if len(body) < 5 {
		return "", fmt.Errorf("响应体过短: %d bytes", len(body))
	}

	// 跳过 gRPC-web 帧头（5 字节: flag + 4字节长度）
	if body[0] != 0x00 {
		return "", fmt.Errorf("非数据帧: flag=0x%02x", body[0])
	}
	msgLen := binary.BigEndian.Uint32(body[1:5])
	if int(msgLen)+5 > len(body) {
		return "", fmt.Errorf("帧长度不匹配: 声明 %d, 实际 %d", msgLen, len(body)-5)
	}
	msg := body[5 : 5+msgLen]

	// 解析 CreateSessionV2Response
	// field 1 (wire type 2) = CreateSessionResponse
	sessionResponseData, err := extractBytesField(msg, 1)
	if err != nil {
		// 可能是 field 2 (ExistingEmailSignInMethods)，邮箱已存在
		if _, err2 := extractBytesField(msg, 2); err2 == nil {
			return "", fmt.Errorf("邮箱已存在（需确认账户）")
		}
		return "", fmt.Errorf("解析 V2Response 失败: %w", err)
	}

	// 解析 CreateSessionResponse
	// field 2 (wire type 2) = session_cookie (string)
	cookie, err := extractStringField(sessionResponseData, 2)
	if err != nil {
		return "", fmt.Errorf("解析 session_cookie 失败: %w", err)
	}
	return cookie, nil
}

// DecodeSessionCookieV1 从 CreateUserAndSession (非V2) 的 gRPC-web 响应中提取 session_cookie
func DecodeSessionCookieV1(body []byte) (string, error) {
	if len(body) < 5 {
		return "", fmt.Errorf("响应体过短: %d bytes", len(body))
	}

	if body[0] != 0x00 {
		return "", fmt.Errorf("非数据帧: flag=0x%02x", body[0])
	}
	msgLen := binary.BigEndian.Uint32(body[1:5])
	if int(msgLen)+5 > len(body) {
		return "", fmt.Errorf("帧长度不匹配")
	}
	msg := body[5 : 5+msgLen]

	// CreateSessionResponse.session_cookie = field 2
	cookie, err := extractStringField(msg, 2)
	if err != nil {
		return "", fmt.Errorf("解析 session_cookie 失败: %w", err)
	}
	return cookie, nil
}

// decodeVarint 解码 varint，返回值和消耗的字节数
func decodeVarint(data []byte) (uint64, int) {
	var val uint64
	var shift uint
	for i, b := range data {
		val |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return val, i + 1
		}
		shift += 7
		if shift >= 64 {
			return 0, 0
		}
	}
	return 0, 0
}

// extractBytesField 从 protobuf 消息中提取指定字段的 bytes 数据
func extractBytesField(msg []byte, targetField int) ([]byte, error) {
	pos := 0
	for pos < len(msg) {
		tag, n := decodeVarint(msg[pos:])
		if n == 0 {
			break
		}
		pos += n

		fieldNum := int(tag >> 3)
		wireType := int(tag & 0x07)

		switch wireType {
		case 0: // varint
			_, n = decodeVarint(msg[pos:])
			if n == 0 {
				return nil, fmt.Errorf("varint 解码失败")
			}
			pos += n
		case 2: // length-delimited
			length, n := decodeVarint(msg[pos:])
			if n == 0 {
				return nil, fmt.Errorf("length 解码失败")
			}
			pos += n
			if pos+int(length) > len(msg) {
				return nil, fmt.Errorf("数据越界: pos=%d, len=%d, msg=%d", pos, length, len(msg))
			}
			if fieldNum == targetField {
				return msg[pos : pos+int(length)], nil
			}
			pos += int(length)
		case 5: // 32-bit
			pos += 4
		case 1: // 64-bit
			pos += 8
		default:
			return nil, fmt.Errorf("未知 wire type: %d", wireType)
		}
	}
	return nil, fmt.Errorf("字段 %d 未找到", targetField)
}

// extractStringField 从 protobuf 消息中提取指定字段的 string 值
func extractStringField(msg []byte, targetField int) (string, error) {
	data, err := extractBytesField(msg, targetField)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
