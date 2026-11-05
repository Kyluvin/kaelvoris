// Package tools 邮件发送工具：email_send —— net/smtp 标准库 SMTP 发送。
//
// 设计约束（零新增依赖）：
//   - 仅用标准库 net/smtp（无第三方邮件库）；支持 SMTP 明文（25/587 端口
//     STARTTLS）与 SSL（465 端口通过 tls.Dial）。
//   - 支持纯文本/HTML 正文、多收件人（To/Cc/Bcc）、附件（嵌入 MIME）。
//   - 凭据配置来自 settings（SMTPHost/Port/User/Pass/From）。
//
// 局限：不支持 OAuth2（Gmail 应用专用密码可用于 smtp.gmail.com:465）。
package tools

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EmailSettings 邮件发送配置（设置页注入）。
type EmailSettings struct {
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	FromAddr string
}

// EmailSendTool SMTP 邮件发送工具。
type EmailSendTool struct {
	// Settings 提供当前邮件配置（nil 或 SMTPHost 空 = 未启用）。
	Settings func() EmailSettings
}

func (t *EmailSendTool) Name() string { return "email_send" }

func (t *EmailSendTool) Description() string {
	return "Send an email via SMTP (plain text or HTML). " +
		"Supports recipients (To/Cc/Bcc), subject, body, and optional attachments. " +
		"Use when the user asks to 发送邮件 / send an email / notify someone by email. " +
		"Requires SMTP configuration (host/port/user/pass/from) in 设置."
}

func (t *EmailSendTool) Parameters() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"to":          strProp("Recipient email address(es), comma-separated or array"),
		"subject":     strProp("Email subject"),
		"body":        strProp("Email body text (plain text)"),
		"html":        strProp("Optional HTML body (overrides plain body when set)"),
		"cc":          strProp("Optional CC recipient(s)"),
		"bcc":         strProp("Optional BCC recipient(s)"),
		"attachments": strProp(`Optional attachment path(s), comma-separated or array (absolute or workspace-relative)`),
	}, []string{"to", "subject"})
}

func (t *EmailSendTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		To          string `json:"to"`
		Subject     string `json:"subject"`
		Body        string `json:"body"`
		HTML        string `json:"html"`
		CC          string `json:"cc"`
		BCC         string `json:"bcc"`
		Attachments string `json:"attachments"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	// 容错：to/attachments 允许 JSON 数组形式。
	if strings.HasPrefix(strings.TrimSpace(p.To), "[") {
		var arr []string
		if err := json.Unmarshal([]byte(p.To), &arr); err == nil {
			p.To = strings.Join(arr, ",")
		}
	}
	if strings.HasPrefix(strings.TrimSpace(p.Attachments), "[") {
		var arr []string
		if err := json.Unmarshal([]byte(p.Attachments), &arr); err == nil {
			p.Attachments = strings.Join(arr, ",")
		}
	}
	if strings.TrimSpace(p.To) == "" || strings.TrimSpace(p.Subject) == "" {
		return "", fmt.Errorf("to and subject are required")
	}
	if t.Settings == nil {
		return "", fmt.Errorf("email is not configured")
	}
	s := t.Settings()
	if strings.TrimSpace(s.SMTPHost) == "" || strings.TrimSpace(s.FromAddr) == "" {
		return "", fmt.Errorf("email is not configured: set SMTP host, port, user, pass, from in 设置")
	}

	toList := splitEmails(p.To)
	ccList := splitEmails(p.CC)
	bccList := splitEmails(p.BCC)
	if len(toList) == 0 {
		return "", fmt.Errorf("at least one valid recipient required")
	}

	// 附件列表。
	var attachPaths []string
	for _, a := range strings.Split(p.Attachments, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			attachPaths = append(attachPaths, a)
		}
	}

	msg, err := buildMIME(s.FromAddr, toList, ccList, bccList, p.Subject, p.Body, p.HTML, attachPaths)
	if err != nil {
		return "", err
	}

	err = smtpSend(ctx, s.SMTPHost, s.SMTPPort, s.SMTPUser, s.SMTPPass, s.FromAddr, toList, ccList, bccList, msg)
	if err != nil {
		return "", fmt.Errorf("send email: %w", err)
	}
	return fmt.Sprintf("email sent: to=%s subject=%q (%d bytes)", strings.Join(toList, ","), p.Subject, len(msg)), nil
}

// splitEmails 逗号/分号分隔邮箱列表（去空）。
func splitEmails(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// buildMIME 构造 RFC 5322 MIME 消息（支持 HTML/附件）。
func buildMIME(from string, to, cc, bcc []string, subject, body, html string, attachPaths []string) ([]byte, error) {
	if html == "" {
		html = body
	}
	bodyText := body
	if bodyText == "" && html != "" {
		bodyText = html
	}

	var b strings.Builder
	boundary := "----=_kaelvoris_" + fmt.Sprint(time.Now().UnixNano())
	writeHeader := func(k, v string) {
		b.WriteString(k + ": " + v + "\r\n")
	}
	writeHeader("From", from)
	writeHeader("To", strings.Join(to, ", "))
	if len(cc) > 0 {
		writeHeader("Cc", strings.Join(cc, ", "))
	}
	if len(bcc) > 0 {
		writeHeader("Bcc", strings.Join(bcc, ", "))
	}
	writeHeader("Subject", mime.QEncoding.Encode("UTF-8", subject))
	writeHeader("Date", time.Now().Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", "multipart/mixed; boundary=\""+boundary+"\"")
	b.WriteString("\r\n")

	// multipart/alternative（text + html）。
	altBoundary := boundary + "_alt"
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + altBoundary + "\"\r\n\r\n")

	b.WriteString("--" + altBoundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(bodyText)) + "\r\n")

	if html != "" {
		b.WriteString("--" + altBoundary + "\r\n")
		b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(html)) + "\r\n")
	}
	b.WriteString("--" + altBoundary + "--\r\n")

	// 附件。
	for _, ap := range attachPaths {
		data, err := os.ReadFile(ap)
		if err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", ap, err)
		}
		fname := filepath.Base(ap)
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(fname)))
		if ct == "" {
			ct = "application/octet-stream"
		}
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + ct + `; name="` + fname + `"` + "\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		b.WriteString("Content-Disposition: attachment; filename=\"" + fname + "\"\r\n\r\n")
		b.WriteString(base64.StdEncoding.EncodeToString(data) + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String()), nil
}

// smtpSend 发送（465 → SSL；其他端口 → STARTTLS 优先，失败回退明文）。
func smtpSend(ctx context.Context, host string, port int, user, pass, from string, to, cc, bcc []string, msg []byte) error {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	allRecipients := append(append(append([]string{}, to...), cc...), bcc...)

	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}

	if port == 465 {
		// SMTPS（SSL）：TLS 直连后 SMTP 对话。
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return fmt.Errorf("tls dial %s: %w", addr, err)
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer client.Close()
		return smtpSendClient(client, auth, from, allRecipients, msg)
	}

	// 25/587：smtp.Dial（自带超时）；STARTTLS 若支持则升级。
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	return smtpSendClient(c, auth, from, allRecipients, msg)
}

// smtpSendClient 在已连接的 SMTP client 上完成认证与发送。
func smtpSendClient(c *smtp.Client, auth smtp.Auth, from string, recipients []string, msg []byte) error {
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, r := range recipients {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("rcpt %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	return c.Quit()
}
