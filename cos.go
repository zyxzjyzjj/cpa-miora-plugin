package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Miora stores reference images in Tencent COS. The web client asks the platform
// for short-lived STS credentials and PUTs straight to the bucket, then feeds the
// resulting object key (fileKey) into the generation call. Uploading is only
// needed for image-to-image / image-to-video / image-to-3D; text-to-* never
// touches this path.
//
// The feature is experimental and disabled by default because it depends on
// Tencent's V5 signature; when it is off, callers must pass an existing Miora
// fileKey instead (miora://file/... or file/...).

const maxReferenceUploadBytes = 20 << 20

type stsCredentials struct {
	SecretID  string `json:"secret_id"`
	SecretKey string `json:"secret_key"`
	Token     string `json:"token"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	Prefix    string `json:"prefix"`
	ExpiredAt int64  `json:"expired_at"`
}

func (c *apiClient) fetchSTS(ctx context.Context) (*stsCredentials, error) {
	data, errFetch := c.requestJSON(ctx, http.MethodGet, pathSTSTokens, nil, nil, true)
	if errFetch != nil {
		return nil, errFetch
	}
	sts := &stsCredentials{}
	if errUnmarshal := json.Unmarshal(data, sts); errUnmarshal != nil {
		return nil, fmt.Errorf("decode sts credentials: %w", errUnmarshal)
	}
	if sts.SecretID == "" || sts.SecretKey == "" || sts.Bucket == "" || sts.Region == "" {
		return nil, fmt.Errorf("STS 凭证不完整，无法上传参考图")
	}
	return sts, nil
}

// uploadReferenceImage uploads one image (data: URL or http(s) URL) and returns
// the Miora fileKey.
func uploadReferenceImage(ctx context.Context, cfg *Config, client *apiClient, image string) (string, error) {
	if client == nil || client.cred == nil || !client.cred.valid() {
		return "", fmt.Errorf("上传参考图需要有效的登录态")
	}
	payload, mime, errDecode := decodeImageReference(ctx, cfg, image)
	if errDecode != nil {
		return "", errDecode
	}
	if len(payload) == 0 {
		return "", fmt.Errorf("参考图内容为空")
	}
	if len(payload) > maxReferenceUploadBytes {
		return "", fmt.Errorf("参考图超过 %d MiB", maxReferenceUploadBytes>>20)
	}

	sts, errSTS := client.fetchSTS(ctx)
	if errSTS != nil {
		return "", errSTS
	}
	ext := extensionForMime(mime)
	objectKey := strings.Trim(strings.TrimSpace(sts.Prefix), "/") + "/" + fmt.Sprintf("%d-%s.%s", time.Now().UnixMilli(), randomHex(4), ext)

	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", sts.Bucket, sts.Region)
	endpoint := "https://" + host + "/" + objectKey

	now := time.Now().Unix()
	authorization := cosAuthorization(sts.SecretID, sts.SecretKey, http.MethodPut, "/"+objectKey, host, mime, now, now+900)

	headers := http.Header{}
	headers.Set("Content-Type", mime)
	headers.Set("Authorization", authorization)
	if sts.Token != "" {
		headers.Set("x-cos-security-token", sts.Token)
	}
	headers.Set("User-Agent", defaultUserAgent)

	result, errPut := doUpstream(ctx, cfg, http.MethodPut, endpoint, headers, payload)
	if errPut != nil {
		return "", errPut
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return "", fmt.Errorf("COS 上传失败（%d）：%s", result.StatusCode, upstreamSnippet(result.Body))
	}
	return objectKey, nil
}

// decodeImageReference converts a client image reference into raw bytes plus a
// MIME type.
func decodeImageReference(ctx context.Context, cfg *Config, image string) ([]byte, string, error) {
	image = strings.TrimSpace(image)
	switch {
	case strings.HasPrefix(image, "data:"):
		header, data, found := strings.Cut(strings.TrimPrefix(image, "data:"), ",")
		if !found {
			return nil, "", fmt.Errorf("data URL 格式不正确")
		}
		mime := strings.TrimSuffix(strings.SplitN(header, ";", 2)[0], ";base64")
		if strings.TrimSpace(mime) == "" {
			mime = "image/png"
		}
		encoding := "base64"
		if parts := strings.Split(header, ";"); len(parts) > 1 {
			encoding = strings.ToLower(strings.TrimSpace(parts[1]))
		}
		if encoding != "base64" {
			return nil, "", fmt.Errorf("只支持 base64 编码的 data URL")
		}
		decoded, errDecode := base64.StdEncoding.DecodeString(data)
		if errDecode != nil {
			return nil, "", fmt.Errorf("data URL base64 解码失败：%w", errDecode)
		}
		return decoded, mime, nil
	case strings.HasPrefix(image, "http://"), strings.HasPrefix(image, "https://"):
		headers := http.Header{}
		headers.Set("User-Agent", defaultUserAgent)
		headers.Set("Accept", "image/*")
		result, errFetch := doUpstream(ctx, cfg, http.MethodGet, image, headers, nil)
		if errFetch != nil {
			return nil, "", errFetch
		}
		if result.StatusCode < 200 || result.StatusCode >= 300 {
			return nil, "", fmt.Errorf("下载参考图失败（%d）", result.StatusCode)
		}
		mime := result.Headers.Get("Content-Type")
		if mime == "" {
			mime = "image/png"
		}
		return result.Body, strings.SplitN(mime, ";", 2)[0], nil
	default:
		return nil, "", fmt.Errorf("无法识别的参考图引用：只支持 data:、http(s):// 或已有的 miora fileKey")
	}
}

func extensionForMime(mime string) string {
	switch strings.ToLower(strings.SplitN(strings.TrimSpace(mime), ";", 2)[0]) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp":
		return "bmp"
	case "image/heic":
		return "heic"
	case "image/avif":
		return "avif"
	case "image/tiff":
		return "tif"
	default:
		ext := strings.TrimPrefix(path.Ext(mime), ".")
		if ext != "" && len(ext) <= 5 {
			return ext
		}
		return "png"
	}
}

// cosAuthorization builds a Tencent COS V5 signed authorization header.
//
//	SignKey     = HMAC-SHA1(SecretKey, KeyTime)
//	HttpString  = method \n path \n query \n headerstring \n
//	StringToSign= "sha1" \n KeyTime \n SHA1(HttpString) \n
//	Signature   = HMAC-SHA1(SignKey, StringToSign)
func cosAuthorization(secretID, secretKey, method, objectPath, host, contentType string, start, end int64) string {
	keyTime := fmt.Sprintf("%d;%d", start, end)
	signKey := hex.EncodeToString(hmacSHA1([]byte(secretKey), []byte(keyTime)))

	headerString := "content-type=" + urlEncode(strings.ToLower(contentType)) + "&host=" + urlEncode(host)
	httpString := strings.ToLower(method) + "\n" + objectPath + "\n" + "\n" + headerString + "\n"
	httpStringHash := hex.EncodeToString(sha1Sum([]byte(httpString)))
	stringToSign := "sha1\n" + keyTime + "\n" + httpStringHash + "\n"
	signature := hex.EncodeToString(hmacSHA1([]byte(signKey), []byte(stringToSign)))

	return "q-sign-algorithm=sha1" +
		"&q-ak=" + secretID +
		"&q-sign-time=" + keyTime +
		"&q-key-time=" + keyTime +
		"&q-header-list=content-type;host" +
		"&q-url-param-list=" +
		"&q-signature=" + signature
}

func hmacSHA1(key, data []byte) []byte {
	mac := hmac.New(sha1.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func sha1Sum(data []byte) []byte {
	sum := sha1.Sum(data)
	return sum[:]
}

// urlEncode matches the escaping Tencent expects for signed header values.
func urlEncode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
