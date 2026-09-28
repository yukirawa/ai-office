package api

// wire.go はサーバ -> クライアントへ送る JSON の形を定義する。
//
// 設計書 §4.2 の固定メッセージ（welcome / notice / task_assign / error）に加え、
// §4.2 の「新しい type の追加は自由」に従って office_state を追加している。
// office_state は TUI（read-only ビューア）が必要とする社員一覧・残高・関係値を
// 1 通でまとめて渡すためのスナップショットで、変化のたびにブロードキャストされる。
// クライアント側は client/crates/protocol/src/lib.rs の ServerMsg と対応する。

// welcomeMsg は hello への応答（§4.2 固定）。
type welcomeMsg struct {
	Type       string     `json:"type"`
	SessionID  string     `json:"session_id"`
	ServerTime string     `json:"server_time"`
	Office     officeWire `json:"office"`
}

// officeWire は welcome.office（§4.2 固定）。
type officeWire struct {
	Online []string `json:"online"`
}

// noticeMsg はチャンネルへの投稿（§4.2 固定）。
type noticeMsg struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
	From    string `json:"from"`
	Text    string `json:"text"`
	TS      string `json:"ts"`
}

// taskAssignMsg はタスク割当（§4.2 固定。payload の中身は Phase 2 以降）。
type taskAssignMsg struct {
	Type    string `json:"type"`
	TaskID  string `json:"task_id"`
	Payload any    `json:"payload"`
}

// errorMsg はエラー応答（§4.2 固定）。
type errorMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// officeStateMsg はオフィス全体のスナップショット（拡張 type）。
type officeStateMsg struct {
	Type          string             `json:"type"`
	Online        []string           `json:"online"`
	Employees     []employeeWire     `json:"employees"`
	Ledger        map[string]int     `json:"ledger"`
	Relationships []relationshipWire `json:"relationships"`
	Tasks         []taskWire         `json:"tasks"`
	TS            string             `json:"ts"`
}

// employeeWire は office_state.employees[] の 1 要素。
type employeeWire struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
	State  string `json:"state"`
}

// relationshipWire は office_state.relationships[] の 1 要素。
type relationshipWire struct {
	FromID   string `json:"from_id"`
	ToID     string `json:"to_id"`
	Affinity int    `json:"affinity"`
	Trust    int    `json:"trust"`
}

// taskWire は office_state.tasks[] の 1 要素（TUI のタスクペイン用）。
type taskWire struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Assignee string `json:"assignee"`
	Mode     string `json:"mode"`
	Result   string `json:"result"`
}

// envelope は「type」だけを先読みするための最小の受信メッセージ。
// 完全な型に落とす前にメッセージ種別を判定するのに使う。
type envelope struct {
	Type string `json:"type"`
}

// helloMsg はクライアントからの自己紹介（§4.1 固定）。
type helloMsg struct {
	Type       string `json:"type"`
	EmployeeID string `json:"employee_id"`
	DeviceID   string `json:"device_id"`
	Version    string `json:"version"`
}

// heartbeatMsg は定期送信の生存通知（§4.1 固定）。
type heartbeatMsg struct {
	Type string `json:"type"`
	TS   string `json:"ts"`
}

// byeMsg は明示的な退勤（§4.1 固定）。
type byeMsg struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// taskResultMsg は worker からの実行結果（§13.1）。
type taskResultMsg struct {
	Type      string        `json:"type"`
	TaskID    string        `json:"task_id"`
	Status    string        `json:"status"`
	Summary   string        `json:"summary"`
	Detail    string        `json:"detail"`
	Artifacts []artifactMsg `json:"artifacts"`
}

// artifactMsg は task_result.artifacts[] の 1 要素。
type artifactMsg struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// taskProgressMsg は worker からの進捗（§13.1）。
type taskProgressMsg struct {
	Type    string `json:"type"`
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
	Percent int    `json:"percent"`
}

// sayMsg は TUI オーナーからの発言（§15.1）。
// channel 省略時はサーバーが #会議室 に寄せる。
type sayMsg struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// taskMsg は TUI オーナーからのタスク投入（§15.1）。title は必須。
type taskMsg struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
	Repo        string `json:"repo"`
	BaseBranch  string `json:"base_branch"`
	Workspace   string `json:"workspace"`
}

// questionMsg は担当者（dev）の疑問を mgr 経由でオーナーへ送る（§16 エスカレーション）。
// クライアント（TUI）はこれを質問として表示し、回答を answer で返す。
type questionMsg struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	From   string `json:"from"`
	Text   string `json:"text"`
	TaskID string `json:"task_id"`
	TS     string `json:"ts"`
}

// answerMsg はオーナーからの回答（§16）。question_id で質問を特定する。
type answerMsg struct {
	Type       string `json:"type"`
	QuestionID string `json:"question_id"`
	Text       string `json:"text"`
}
