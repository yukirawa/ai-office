package api

// ws.go は WebSocket の接続ハンドラとブロードキャスト用 Hub を実装する。
//
// 設計書 §4 のプロトコルを喋る。/ws に接続したクライアントは、まず hello を送る。
// employees に存在する ID なら出勤（check-in）として扱い、存在しない ID は
// 観測者（observer、TUI など）として扱って在席には数えない。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/agents"
)

const (
	// maxMessageSize は 1 メッセージの上限バイト数。
	maxMessageSize = 1 << 20
	// sendBuffer はクライアントごとの送信バッファ。溢れたら切断せずドロップする。
	sendBuffer = 64
	// helloTimeout は接続後に hello を待つ時間。
	helloTimeout = 10 * time.Second
	// channelDefault はシステム通知の投稿先。
	channelDefault = "#会議室"
	// notifyFromDefault はシステム通知の差出人 ID。
	notifyFromDefault = "system"
	// historyLimit は接続時に送る直近メッセージ数。
	historyLimit = 50
)

// client は WebSocket 接続 1 本を表す。
type client struct {
	conn       *websocket.Conn
	send       chan []byte
	employeeID string
	// known は employees に存在する社員として check-in したかどうか。
	known bool
}

// writeLoop は send に積まれたメッセージを順に書き出す。send が閉じると終了する。
// nhooyr の Conn は「読み 1 本・書き 1 本」の並行利用を許すため、
// 読み取りはハンドラ goroutine、書き込みはこの goroutine が担当する。
func (c *client) writeLoop() {
	for data := range c.send {
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.conn.Write(wctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			return
		}
	}
}

// hub は接続中クライアントの集合。ブロードキャストを担当する。
type hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[*client]struct{})}
}

func (h *hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

// remove はクライアントを取り除く。戻った後に broadcast がこのクライアントへ
// 送ることはない（RLock 保持中の送信が完了するのを待ってからロックを取るため）。
func (h *hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// broadcast は全クライアントへ同じメッセージを送る。遅いクライアントは
// バッファが満杯ならスキップする（1 本の詰まりで全体を止めない）。
func (h *hub) broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- b:
		default:
		}
	}
}

// evictEmployee は指定社員として接続中のクライアントを閉じる（新しい接続で置き換える）。
// except には置き換え元の新クライアントを渡し、自分自身は閉じない。
// 同じ社員 ID の worker が二重に接続したままになると、古いプロセスにタスクが飛んで
// 設定違い（例: OFFICE_ALLOW_EXEC 未設定）で失敗するため、明示的に切断する。
func (h *hub) evictEmployee(employeeID string, except *client) {
	h.mu.RLock()
	var victims []*client
	for c := range h.clients {
		if c == except {
			continue
		}
		if c.known && c.employeeID == employeeID {
			victims = append(victims, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range victims {
		// Close は close ハンドシェイクを待つため、新接続をブロックしないよう goroutine で実行する。
		victim := c
		go func() {
			_ = victim.conn.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
		}()
	}
}

// hasClients は接続中のクライアントが 1 つ以上あるかを返す。
func (h *hub) hasClients() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients) > 0
}

// byEmployee は指定社員として接続中のクライアントを返す（observer は対象外）。
func (h *hub) byEmployee(employeeID string) *client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.known && c.employeeID == employeeID {
			return c
		}
	}
	return nil
}

// sendJSONTo は 1 クライアントへ直接メッセージを送る。バッファ満杯なら false。
func (h *hub) sendJSONTo(c *client, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	select {
	case c.send <- b:
		return true
	default:
		return false
	}
}

// handleWS は /ws のハンドラ。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 設計書 §2: Tailscale 内に閉じる前提。オリジン検査は行わない。
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Warn("WebSocket の accept に失敗しました", "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxMessageSize)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hello, ok := s.readHello(ctx, conn)
	if !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}

	cl := &client{conn: conn, send: make(chan []byte, sendBuffer), employeeID: hello.EmployeeID}
	s.hub.add(cl)
	go cl.writeLoop()

	info := s.resolveHello(hello)
	cl.known = info.known
	// welcome の office.online に自分を含めるため、先に在席へ反映する。
	if info.known {
		// 同一社員の古い接続が残っていれば置き換える（旧 worker にタスクが飛ぶのを防ぐ）。
		s.hub.evictEmployee(hello.EmployeeID, cl)
		s.presence.CheckIn(hello.EmployeeID, hello.DeviceID)
	}

	// welcome -> 現在のスナップショット -> 直近ログの順に送る。
	s.sendJSON(cl, welcomeMsg{
		Type:       "welcome",
		SessionID:  info.sessionID,
		ServerTime: formatTS(time.Now().UTC()),
		Office:     officeWire{Online: nonNil(s.presence.Online())},
	})
	s.sendJSON(cl, s.snapshot(context.Background()))
	s.sendHistory(cl)

	if info.known {
		s.announceCheckIn(ctx, hello, info)
	}

	reason := s.readLoop(ctx, cl)

	// 送信を止めてからバッファを閉じる（broadcast との競合を防ぐ）。
	s.hub.remove(cl)
	close(cl.send)

	if info.known {
		s.failWaiters(cl.employeeID, reason)
		// 同一社員の別接続がまだ在席なら、置き換え時の誤った退勤処理をしない。
		if s.hub.byEmployee(cl.employeeID) == nil {
			s.checkOut(ctx, cl.employeeID, info.name, reason)
		} else {
			s.log.Info("同一社員の別接続が在席のため退勤処理をスキップします", "employee_id", cl.employeeID)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	s.log.Info("接続を終了しました", "employee_id", cl.employeeID, "reason", reason)
}

// readHello は接続後はじめの hello を読む。不正なら error メッセージを送って false。
func (s *Server) readHello(ctx context.Context, conn *websocket.Conn) (helloMsg, bool) {
	rctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()

	typ, data, err := conn.Read(rctx)
	if err != nil {
		s.log.Info("hello の受信に失敗しました", "error", err)
		return helloMsg{}, false
	}
	if typ != websocket.MessageText {
		s.writeErrorDirect(ctx, conn, "invalid_hello", "expected hello (text message)")
		return helloMsg{}, false
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		s.writeErrorDirect(ctx, conn, "invalid_hello", "expected hello")
		return helloMsg{}, false
	}
	if env.Type != "hello" {
		s.writeErrorDirect(ctx, conn, "invalid_hello", "expected hello")
		return helloMsg{}, false
	}

	var h helloMsg
	if err := json.Unmarshal(data, &h); err != nil {
		s.writeErrorDirect(ctx, conn, "invalid_hello", "malformed hello")
		return helloMsg{}, false
	}
	h.EmployeeID = strings.TrimSpace(h.EmployeeID)
	if h.EmployeeID == "" {
		s.writeErrorDirect(ctx, conn, "invalid_hello", "employee_id is required")
		return helloMsg{}, false
	}
	if h.DeviceID == "" {
		h.DeviceID = "unknown"
	}
	return h, true
}

// helloInfo は hello の解決結果。
type helloInfo struct {
	// known は employees に存在する社員として出勤したかどうか。
	known bool
	// name は表示名（observer の場合は employee_id）。
	name string
	// sessionID は welcome で返す session_id。
	sessionID string
}

// resolveHello は hello を社員として解決し、セッションを開く。
// employees に無い ID は observer として扱い、在席には数えない。
func (s *Server) resolveHello(h helloMsg) helloInfo {
	sessionID := uuid.NewString()

	rec, err := s.store.Employee(h.EmployeeID)
	if errors.Is(err, sql.ErrNoRows) {
		s.log.Info("未登録の接続を observer として扱います",
			"employee_id", h.EmployeeID, "device_id", h.DeviceID, "version", h.Version)
		return helloInfo{known: false, name: h.EmployeeID, sessionID: sessionID}
	}
	if err != nil {
		s.log.Error("社員の取得に失敗しました", "employee_id", h.EmployeeID, "error", err)
		return helloInfo{known: false, name: h.EmployeeID, sessionID: sessionID}
	}

	// 再接続を想定し、残っているセッションを閉じてから新しいセッションを開く。
	if err := s.store.EndActiveSessions(h.EmployeeID, "reconnect"); err != nil {
		s.log.Warn("旧セッションの終了に失敗しました", "employee_id", h.EmployeeID, "error", err)
	}
	if err := s.store.StartSession(sessionID, h.EmployeeID, h.DeviceID); err != nil {
		s.log.Warn("セッションの開始に失敗しました", "employee_id", h.EmployeeID, "error", err)
	}
	return helloInfo{known: true, name: rec.Name, sessionID: sessionID}
}

// announceCheckIn は出勤ログと通知の配信、状態ブロードキャストを行う。
// welcome を先に送るため、在席反映（presence.CheckIn）と分離している。
func (s *Server) announceCheckIn(ctx context.Context, h helloMsg, info helloInfo) {
	s.log.Info("check-in", "employee_id", h.EmployeeID, "name", info.name,
		"device_id", h.DeviceID, "session_id", info.sessionID)
	if err := s.Notify(ctx, channelDefault, notifyFromDefault,
		fmt.Sprintf("%s (%s) が出勤しました", info.name, h.EmployeeID)); err != nil {
		s.log.Warn("出勤通知の投稿に失敗しました", "error", err)
	}
	s.BroadcastState()
}

// checkOut は退勤処理を行う。
func (s *Server) checkOut(ctx context.Context, id, name, reason string) {
	s.presence.CheckOut(id)
	if err := s.store.EndActiveSessions(id, reason); err != nil {
		s.log.Warn("セッションの終了に失敗しました", "employee_id", id, "error", err)
	}
	s.log.Info("check-out", "employee_id", id, "name", name, "reason", reason)

	if err := s.Notify(ctx, channelDefault, notifyFromDefault,
		fmt.Sprintf("%s (%s) が退勤しました", name, id)); err != nil {
		s.log.Warn("退勤通知の投稿に失敗しました", "error", err)
	}
	s.BroadcastState()
}

// readLoop は受信メッセージを種別ごとに処理する。接続が切れるか bye を受けると理由を返す。
func (s *Server) readLoop(ctx context.Context, cl *client) string {
	for {
		typ, data, err := cl.conn.Read(ctx)
		if err != nil {
			return "disconnect"
		}
		if typ != websocket.MessageText {
			continue
		}

		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.log.Warn("不正なメッセージを無視しました", "employee_id", cl.employeeID, "error", err)
			continue
		}

		switch env.Type {
		case "heartbeat":
			// observer は在席管理の対象外なので何もしない。
			if cl.known {
				s.presence.Heartbeat(cl.employeeID)
			}
		case "bye":
			var b byeMsg
			_ = json.Unmarshal(data, &b)
			s.log.Info("bye を受信しました", "employee_id", cl.employeeID, "reason", b.Reason)
			return "bye"
		case "task_result":
			var res taskResultMsg
			if err := json.Unmarshal(data, &res); err != nil {
				s.log.Warn("task_result の解釈に失敗しました", "employee_id", cl.employeeID, "error", err)
				continue
			}
			s.log.Info("task_result を受信しました",
				"task_id", res.TaskID, "status", res.Status, "employee_id", cl.employeeID)
			s.deliverResult(agents.Result{
				TaskID:  res.TaskID,
				Status:  res.Status,
				Summary: res.Summary,
				Detail:  res.Detail,
			})
		case "task_progress":
			var pg taskProgressMsg
			if err := json.Unmarshal(data, &pg); err != nil {
				continue
			}
			s.log.Info("task_progress を受信しました",
				"task_id", pg.TaskID, "percent", pg.Percent, "message", pg.Message)
		case "say":
			var sm sayMsg
			if err := json.Unmarshal(data, &sm); err != nil {
				s.log.Warn("say の解釈に失敗しました", "employee_id", cl.employeeID, "error", err)
				continue
			}
			text := strings.TrimSpace(sm.Text)
			if text == "" {
				s.sendError(cl, "invalid_say", "text が空です")
				continue
			}
			channel := strings.TrimSpace(sm.Channel)
			if channel == "" {
				channel = channelDefault
			}
			// 永続化 + notice 配信。失敗しても接続は維持する。
			if err := s.Notify(ctx, channel, cl.employeeID, text); err != nil {
				s.log.Warn("say の投稿に失敗しました", "employee_id", cl.employeeID, "error", err)
			}
			// @宛先（@mgr / @dev_m / @chat / @all）に応じて会話を振り分ける。
			// メンション無しは chat 役に渡す（§15.1）。
			target, body := parseMention(text)
			s.dispatchMention(channel, target, body)
			s.log.Info("say を受信しました",
				"employee_id", cl.employeeID, "channel", channel, "target", target,
				"text", truncateRunes(text, 80))
		case "task":
			var tm taskMsg
			if err := json.Unmarshal(data, &tm); err != nil {
				s.log.Warn("task の解釈に失敗しました", "employee_id", cl.employeeID, "error", err)
				continue
			}
			title := strings.TrimSpace(tm.Title)
			if title == "" {
				s.sendError(cl, "invalid_task", "title は必須です")
				continue
			}
			id, err := s.CreateTask(ctx, CreateTaskInput{
				Title:       title,
				Description: tm.Description,
				From:        cl.employeeID,
				Mode:        tm.Mode,
				Repo:        tm.Repo,
				BaseBranch:  tm.BaseBranch,
				Workspace:   tm.Workspace,
			})
			if err != nil {
				s.sendError(cl, "task_failed", err.Error())
				continue
			}
			s.log.Info("task を受け付けました", "task_id", id, "employee_id", cl.employeeID)
		case "answer":
			var am answerMsg
			if err := json.Unmarshal(data, &am); err != nil {
				s.log.Warn("answer の解釈に失敗しました", "employee_id", cl.employeeID, "error", err)
				continue
			}
			text := strings.TrimSpace(am.Text)
			if text == "" {
				s.sendError(cl, "invalid_answer", "text が空です")
				continue
			}
			qid := strings.TrimSpace(am.QuestionID)
			// 回答を #会議室 に投稿して可視化する（from は送信者 = 通常 owner）。
			if err := s.Notify(ctx, channelDefault, cl.employeeID, "【回答】"+text); err != nil {
				s.log.Warn("回答の投稿に失敗しました", "employee_id", cl.employeeID, "error", err)
			}
			s.AnswerQuestion(qid, text)
			s.log.Info("answer を受信しました", "employee_id", cl.employeeID, "question_id", qid)
		case "hello":
			s.sendError(cl, "duplicate_hello", "hello は接続時に 1 回だけ送ってください")
		default:
			s.log.Debug("未知のメッセージを無視しました", "type", env.Type, "employee_id", cl.employeeID)
		}
	}
}

// sendJSON は 1 クライアントへメッセージを送る（バッファ満杯ならドロップ）。
func (s *Server) sendJSON(cl *client, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Error("メッセージのシリアライズに失敗しました", "error", err)
		return
	}
	select {
	case cl.send <- b:
	default:
		s.log.Warn("送信バッファが満杯のためドロップしました", "employee_id", cl.employeeID)
	}
}

// sendError は 1 クライアントへ error メッセージを送る。
func (s *Server) sendError(cl *client, code, message string) {
	s.sendJSON(cl, errorMsg{Type: "error", Code: code, Message: message})
}

// writeErrorDirect は Hub に載る前（hello 失敗時）のエラー送信。
func (s *Server) writeErrorDirect(ctx context.Context, conn *websocket.Conn, code, message string) {
	b, err := json.Marshal(errorMsg{Type: "error", Code: code, Message: message})
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, b)
}

// sendHistory は接続直後に直近のチャンネルログを notice として送る。
// TUI が「今の会議室」を空から始めないための拡張。
func (s *Server) sendHistory(cl *client) {
	msgs, err := s.store.Messages(channelDefault, historyLimit)
	if err != nil {
		s.log.Warn("履歴の取得に失敗しました", "error", err)
		return
	}
	for _, m := range msgs {
		// 過去の出退勤（system の入退室）は履歴に出さない。
		// 毎回の接続で大量に再生されて見づらくなるため（特に再起動後）。
		if isPresenceNoise(m.FromID, m.Content) {
			continue
		}
		s.sendJSON(cl, noticeMsg{
			Type:    "notice",
			Channel: m.Channel,
			From:    m.FromID,
			Text:    m.Content,
			TS:      formatTS(m.TS),
		})
	}
}

// isPresenceNoise は履歴から除外すべき入退室通知かを判定する。
func isPresenceNoise(fromID, content string) bool {
	if fromID != notifyFromDefault {
		return false
	}
	switch {
	case strings.Contains(content, "出勤しました"),
		strings.Contains(content, "退勤しました"),
		strings.Contains(content, "退勤扱いにしました"):
		return true
	default:
		return false
	}
}

// truncateRunes はログ用に s を最大 n ルーンへ切り詰める。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// nonNil は nil スライスを空スライスに正規化する（JSON で null にしない）。
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
