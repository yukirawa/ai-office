//! ai-office ワイヤプロトコル（設計書 §4）。
//!
//! WebSocket 上の JSON、UTF-8、`"type"` で内部タグ付け、フィールド名は snake_case。
//! 追加フィールドは無視され、未知の `type` は [`ServerMsg::Unknown`] にフォールバックする
//! （前方互換）。

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

/// Client -> Server メッセージ（§4.1）。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ClientMsg {
    /// 接続直後に1回送る自己紹介。
    Hello {
        employee_id: String,
        device_id: String,
        version: String,
    },
    /// 定期送信する生存通知。
    Heartbeat { ts: String },
    /// 明示的な退勤。
    Bye { reason: String },
    /// タスクの実行結果（§13.1、Phase 2.2）。`status` は `done` | `failed`。
    TaskResult {
        task_id: String,
        status: String,
        summary: String,
        detail: String,
        #[serde(default)]
        artifacts: Vec<Artifact>,
    },
    /// 実行中の進捗（§13.1、任意）。
    TaskProgress {
        task_id: String,
        message: String,
        percent: u8,
    },
    /// #会議室 への発言（§15.1）。`channel` は通常 `#会議室`。
    Say { channel: String, text: String },
    /// タスク投入（§15.1、サーバー側の `POST /api/tasks` と同じ経路）。
    ///
    /// `title` 以外は省略可能（省略時は既定値。`mode` は呼び側で `local` を推奨）。
    Task {
        title: String,
        #[serde(default)]
        description: String,
        #[serde(default)]
        mode: String,
        #[serde(default)]
        repo: String,
        #[serde(default)]
        base_branch: String,
    },
    /// 質問への回答（新機能）。`question_id` は [`ServerMsg::Question`] の `id`。
    Answer { question_id: String, text: String },
}

impl ClientMsg {
    /// コンパクトな JSON 文字列へシリアライズする。
    ///
    /// この型は常にシリアライズ可能なので panic しない。
    pub fn to_json(&self) -> String {
        serde_json::to_string(self).expect("ClientMsg is always JSON-serializable")
    }
}

/// Server -> Client メッセージ（§4.2）。
///
/// 未知の `type` は [`ServerMsg::Unknown`] になる。`Unknown` は必ず最後の variant に置くこと
/// （`#[serde(other)]` は最後にしか置けない）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ServerMsg {
    /// `hello` への応答。
    Welcome {
        session_id: String,
        server_time: String,
        office: Office,
    },
    /// チャンネルへの投稿。
    Notice {
        channel: String,
        from: String,
        text: String,
        ts: String,
    },
    /// タスク割当（payload の中身は Phase 2 以降で確定）。
    TaskAssign {
        task_id: String,
        #[serde(default)]
        payload: serde_json::Value,
    },
    /// エラー応答。
    Error { code: String, message: String },
    /// 割り当て済みタスクの取り消し（§13.2、予約）。
    TaskCancel { task_id: String, reason: String },
    /// オフィス全体のスナップショット。
    OfficeState {
        #[serde(default)]
        online: Vec<String>,
        #[serde(default)]
        employees: Vec<EmployeeInfo>,
        #[serde(default)]
        ledger: BTreeMap<String, i64>,
        #[serde(default)]
        relationships: Vec<RelationshipInfo>,
        /// タスク一覧。新しい順（newest-first）で、省略・空がありうる（§13.4 追補）。
        #[serde(default)]
        tasks: Vec<TaskInfo>,
        ts: String,
    },
    /// 平社員からの質問（mgr 経由、新機能）。オーナーは [`ClientMsg::Answer`] で回答する。
    Question {
        id: String,
        #[serde(default)]
        from: String,
        #[serde(default)]
        text: String,
        #[serde(default)]
        task_id: String,
        #[serde(default)]
        ts: String,
    },
    /// 未知の `type`。前方互換のためのフォールバック。
    #[serde(other)]
    Unknown,
}

impl ServerMsg {
    /// JSON 文字列をパースする。
    pub fn parse(s: &str) -> serde_json::Result<ServerMsg> {
        serde_json::from_str(s)
    }
}

/// `welcome.office`。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct Office {
    #[serde(default)]
    pub online: Vec<String>,
}

/// `office_state.employees[]`。`state` は省略時は空文字。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct EmployeeInfo {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub role: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub state: String,
}

/// `office_state.relationships[]`。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct RelationshipInfo {
    #[serde(default)]
    pub from_id: String,
    #[serde(default)]
    pub to_id: String,
    #[serde(default)]
    pub affinity: i64,
    #[serde(default)]
    pub trust: i64,
}

/// `office_state.tasks[]`（§13.4 追補）。
///
/// 全フィールド省略可能（前方/後方互換）。`mode` は `local` | `remote`、
/// `status` は §13.3 の状態遷移（`pending` / `assigned` / `working` / `review` / `done` / `failed`）を想定。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct TaskInfo {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub assignee: String,
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub result: String,
}

/// local モードの 1 操作（§13.2）。
///
/// `op` は `read` | `write` | `list` | `exec`。用途が異なるフィールドは省略可能
/// （例: `write` は `path`+`content`、`exec` は `cmd`+`args`+`cwd`）。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct Action {
    pub op: String,
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub content: String,
    #[serde(default)]
    pub cmd: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub cwd: String,
}

/// remote モードでプッシュする 1 ファイル（§13.2）。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct RemoteFile {
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub content: String,
}

/// remote モードの GitHub 操作指定（§13.2）。`token` は省略・空がありうる。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct RemoteSpec {
    #[serde(default)]
    pub repo: String,
    #[serde(default)]
    pub base_branch: String,
    #[serde(default)]
    pub branch: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub files: Vec<RemoteFile>,
    #[serde(default)]
    pub token: String,
}

/// `task_assign.payload` を確定させたもの（§13.2、Phase 2.2）。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct TaskAssignPayload {
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub actions: Vec<Action>,
    #[serde(default)]
    pub remote: Option<RemoteSpec>,
}

/// `task_result.artifacts[]`。書き込んだファイルとバイト数。
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct Artifact {
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub bytes: u64,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hello_round_trip_is_exact() {
        let msg = ClientMsg::Hello {
            employee_id: "dev_m".to_string(),
            device_id: "zenbook".to_string(),
            version: "0.1.0".to_string(),
        };
        let json = msg.to_json();
        assert_eq!(
            json,
            r#"{"type":"hello","employee_id":"dev_m","device_id":"zenbook","version":"0.1.0"}"#
        );
        // 文字列 -> 値のラウンドトリップ。
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn heartbeat_round_trip_is_exact() {
        let msg = ClientMsg::Heartbeat {
            ts: "2026-09-26T12:34:56Z".to_string(),
        };
        let json = msg.to_json();
        assert_eq!(json, r#"{"type":"heartbeat","ts":"2026-09-26T12:34:56Z"}"#);
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn bye_round_trip_is_exact() {
        let msg = ClientMsg::Bye {
            reason: "shutdown".to_string(),
        };
        let json = msg.to_json();
        assert_eq!(json, r#"{"type":"bye","reason":"shutdown"}"#);
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn parse_welcome() {
        let json = r#"{
            "type": "welcome",
            "session_id": "uuid-v4",
            "server_time": "2026-09-26T12:34:56Z",
            "office": { "online": ["mgr", "dev_m"] }
        }"#;
        let msg = ServerMsg::parse(json).unwrap();
        match msg {
            ServerMsg::Welcome {
                session_id,
                server_time,
                office,
            } => {
                assert_eq!(session_id, "uuid-v4");
                assert_eq!(server_time, "2026-09-26T12:34:56Z");
                assert_eq!(office.online, vec!["mgr", "dev_m"]);
            }
            other => panic!("expected Welcome, got {other:?}"),
        }
    }

    #[test]
    fn parse_notice() {
        let json = r##"{
            "type": "notice",
            "channel": "#会議室",
            "from": "mgr",
            "text": "今日のタスクは…",
            "ts": "2026-09-26T12:34:56Z"
        }"##;
        let msg = ServerMsg::parse(json).unwrap();
        assert_eq!(
            msg,
            ServerMsg::Notice {
                channel: "#会議室".to_string(),
                from: "mgr".to_string(),
                text: "今日のタスクは…".to_string(),
                ts: "2026-09-26T12:34:56Z".to_string(),
            }
        );
    }

    #[test]
    fn parse_office_state() {
        let json = r#"{
            "type": "office_state",
            "online": ["mgr"],
            "employees": [
                {"id":"mgr","name":"マネージャー","role":"manager","status":"online","state":"idle"}
            ],
            "ledger": {"mgr": 512, "dev_m": 350},
            "relationships": [
                {"from_id":"mgr","to_id":"dev_f","affinity":15,"trust":70}
            ],
            "tasks": [
                {"id":"t1","title":"レポート作成","status":"done","assignee":"dev_m","mode":"local","result":"ok"}
            ],
            "ts": "2026-09-26T12:34:56Z"
        }"#;
        let msg = ServerMsg::parse(json).unwrap();
        match msg {
            ServerMsg::OfficeState {
                online,
                employees,
                ledger,
                relationships,
                tasks,
                ts,
            } => {
                assert_eq!(online, vec!["mgr"]);
                assert_eq!(employees.len(), 1);
                assert_eq!(employees[0].id, "mgr");
                assert_eq!(employees[0].state, "idle");
                assert_eq!(ledger.get("mgr"), Some(&512));
                assert_eq!(ledger.get("dev_m"), Some(&350));
                assert_eq!(relationships.len(), 1);
                assert_eq!(relationships[0].from_id, "mgr");
                assert_eq!(relationships[0].to_id, "dev_f");
                assert_eq!(relationships[0].affinity, 15);
                assert_eq!(relationships[0].trust, 70);
                assert_eq!(tasks.len(), 1);
                assert_eq!(tasks[0].id, "t1");
                assert_eq!(tasks[0].status, "done");
                assert_eq!(ts, "2026-09-26T12:34:56Z");
            }
            other => panic!("expected OfficeState, got {other:?}"),
        }
    }

    #[test]
    fn partial_office_state_is_accepted() {
        // コレクション系フィールドは省略可能。未知フィールドも無視される。
        let json = r#"{"type":"office_state","ts":"x","future_field":123}"#;
        let msg = ServerMsg::parse(json).unwrap();
        match msg {
            ServerMsg::OfficeState {
                online,
                employees,
                ledger,
                relationships,
                tasks,
                ts,
            } => {
                assert!(online.is_empty());
                assert!(employees.is_empty());
                assert!(ledger.is_empty());
                assert!(relationships.is_empty());
                assert!(tasks.is_empty());
                assert_eq!(ts, "x");
            }
            other => panic!("expected OfficeState, got {other:?}"),
        }
    }

    #[test]
    fn employee_state_defaults_to_empty() {
        let e: EmployeeInfo = serde_json::from_str(r#"{"id":"mgr"}"#).unwrap();
        assert_eq!(e.id, "mgr");
        assert_eq!(e.state, "");
        assert_eq!(e.name, "");
    }

    #[test]
    fn parse_office_state_with_tasks() {
        // tasks は新しい順。全フィールドを読み取れること。
        let json = r#"{
            "type": "office_state",
            "online": ["dev_m"],
            "tasks": [
                {"id":"t1","title":"レポート作成","status":"done","assignee":"dev_m","mode":"local","result":"done in 3s"},
                {"id":"t2","title":"調査","status":"working","assignee":"dev_f","mode":"remote","result":""}
            ],
            "ts": "t"
        }"#;
        match ServerMsg::parse(json).unwrap() {
            ServerMsg::OfficeState { tasks, .. } => {
                assert_eq!(tasks.len(), 2);
                assert_eq!(tasks[0].id, "t1");
                assert_eq!(tasks[0].title, "レポート作成");
                assert_eq!(tasks[0].status, "done");
                assert_eq!(tasks[0].assignee, "dev_m");
                assert_eq!(tasks[0].mode, "local");
                assert_eq!(tasks[0].result, "done in 3s");
                assert_eq!(tasks[1].id, "t2");
                assert_eq!(tasks[1].status, "working");
                assert_eq!(tasks[1].mode, "remote");
            }
            other => panic!("expected OfficeState, got {other:?}"),
        }
    }

    #[test]
    fn office_state_without_tasks_is_empty() {
        // tasks が無くてもパースでき、空 vec になる（後方互換）。
        let json = r#"{"type":"office_state","online":["mgr"],"ts":"x"}"#;
        match ServerMsg::parse(json).unwrap() {
            ServerMsg::OfficeState { tasks, .. } => assert!(tasks.is_empty()),
            other => panic!("expected OfficeState, got {other:?}"),
        }
    }

    #[test]
    fn task_info_fields_default_to_empty() {
        // 未知/省略フィールドは空文字にフォールバックする。
        let t: TaskInfo = serde_json::from_str(r#"{"id":"t1"}"#).unwrap();
        assert_eq!(t.id, "t1");
        assert_eq!(t.title, "");
        assert_eq!(t.status, "");
        assert_eq!(t.assignee, "");
        assert_eq!(t.mode, "");
        assert_eq!(t.result, "");
    }

    #[test]
    fn unknown_type_falls_back() {
        let msg = ServerMsg::parse(r#"{"type":"something_new","foo":1}"#).unwrap();
        assert_eq!(msg, ServerMsg::Unknown);
        // 追加フィールドは無視される。
        let welcome = ServerMsg::parse(
            r#"{"type":"welcome","session_id":"s","server_time":"t","office":{"online":[]},"extra":true}"#,
        )
        .unwrap();
        assert!(matches!(welcome, ServerMsg::Welcome { .. }));
    }

    #[test]
    fn parse_error_and_task_assign() {
        assert_eq!(
            ServerMsg::parse(
                r#"{"type":"error","code":"invalid_hello","message":"expected hello"}"#
            )
            .unwrap(),
            ServerMsg::Error {
                code: "invalid_hello".to_string(),
                message: "expected hello".to_string(),
            }
        );
        assert_eq!(
            ServerMsg::parse(r#"{"type":"task_assign","task_id":"t1","payload":{}}"#).unwrap(),
            ServerMsg::TaskAssign {
                task_id: "t1".to_string(),
                payload: serde_json::json!({}),
            }
        );
    }

    #[test]
    fn task_result_json_is_exact() {
        let msg = ClientMsg::TaskResult {
            task_id: "t1".to_string(),
            status: "done".to_string(),
            summary: "3 アクションを実行しました".to_string(),
            detail: "log".to_string(),
            artifacts: vec![Artifact {
                path: "reports/x.md".to_string(),
                bytes: 123,
            }],
        };
        let json = msg.to_json();
        assert_eq!(
            json,
            r#"{"type":"task_result","task_id":"t1","status":"done","summary":"3 アクションを実行しました","detail":"log","artifacts":[{"path":"reports/x.md","bytes":123}]}"#
        );
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn task_progress_json_is_exact() {
        let msg = ClientMsg::TaskProgress {
            task_id: "t1".to_string(),
            message: "working".to_string(),
            percent: 50,
        };
        let json = msg.to_json();
        assert_eq!(
            json,
            r#"{"type":"task_progress","task_id":"t1","message":"working","percent":50}"#
        );
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn say_json_is_exact() {
        let msg = ClientMsg::Say {
            channel: "#会議室".to_string(),
            text: "おはよう".to_string(),
        };
        let json = msg.to_json();
        assert_eq!(
            json,
            r##"{"type":"say","channel":"#会議室","text":"おはよう"}"##
        );
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn answer_json_is_exact() {
        let msg = ClientMsg::Answer {
            question_id: "q-1".to_string(),
            text: "その方針で進めてください".to_string(),
        };
        let json = msg.to_json();
        assert_eq!(
            json,
            r#"{"type":"answer","question_id":"q-1","text":"その方針で進めてください"}"#
        );
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn parse_question() {
        let json = r#"{
            "type": "question",
            "id": "q-1",
            "from": "dev_m",
            "text": "確認したいこと",
            "task_id": "t-1",
            "ts": "2026-09-28T09:00:00Z"
        }"#;
        assert_eq!(
            ServerMsg::parse(json).unwrap(),
            ServerMsg::Question {
                id: "q-1".to_string(),
                from: "dev_m".to_string(),
                text: "確認したいこと".to_string(),
                task_id: "t-1".to_string(),
                ts: "2026-09-28T09:00:00Z".to_string(),
            }
        );
    }

    #[test]
    fn question_optional_fields_default_to_empty() {
        // 省略可能な文字列フィールドは空にフォールバックする（前方互換）。
        let msg = ServerMsg::parse(r#"{"type":"question","id":"q-1"}"#).unwrap();
        assert_eq!(
            msg,
            ServerMsg::Question {
                id: "q-1".to_string(),
                from: String::new(),
                text: String::new(),
                task_id: String::new(),
                ts: String::new(),
            }
        );
    }

    #[test]
    fn task_json_is_exact() {
        let msg = ClientMsg::Task {
            title: "レポート作成".to_string(),
            description: String::new(),
            mode: "local".to_string(),
            repo: String::new(),
            base_branch: String::new(),
        };
        let json = msg.to_json();
        assert_eq!(
            json,
            r#"{"type":"task","title":"レポート作成","description":"","mode":"local","repo":"","base_branch":""}"#
        );
        assert_eq!(serde_json::from_str::<ClientMsg>(&json).unwrap(), msg);
    }

    #[test]
    fn task_optional_fields_can_be_omitted() {
        // title 以外は省略でき、既定値（空文字）になる。
        let parsed: ClientMsg = serde_json::from_str(r#"{"type":"task","title":"A"}"#).unwrap();
        assert_eq!(
            parsed,
            ClientMsg::Task {
                title: "A".to_string(),
                description: String::new(),
                mode: String::new(),
                repo: String::new(),
                base_branch: String::new(),
            }
        );
    }

    #[test]
    fn parse_task_assign_local_payload() {
        let json = r#"{
            "type": "task_assign",
            "task_id": "t1",
            "payload": {
                "mode": "local",
                "title": "レポート作成",
                "reason": "mgr の計画",
                "actions": [
                    {"op": "write", "path": "reports/x.md", "content": "hello"}
                ]
            }
        }"#;
        let msg = ServerMsg::parse(json).unwrap();
        let ServerMsg::TaskAssign { task_id, payload } = msg else {
            panic!("expected TaskAssign");
        };
        assert_eq!(task_id, "t1");
        let payload: TaskAssignPayload = serde_json::from_value(payload).unwrap();
        assert_eq!(payload.mode, "local");
        assert_eq!(payload.title, "レポート作成");
        assert_eq!(payload.reason, "mgr の計画");
        assert!(payload.remote.is_none());
        assert_eq!(payload.actions.len(), 1);
        assert_eq!(payload.actions[0].op, "write");
        assert_eq!(payload.actions[0].path, "reports/x.md");
        assert_eq!(payload.actions[0].content, "hello");
        // 未設定フィールドは既定値にフォールバックする。
        assert!(payload.actions[0].args.is_empty());
        assert_eq!(payload.actions[0].cwd, "");
    }

    #[test]
    fn parse_task_assign_remote_payload() {
        let json = r#"{
            "type": "task_assign",
            "task_id": "t2",
            "payload": {
                "mode": "remote",
                "title": "PR",
                "remote": {
                    "repo": "owner/repo",
                    "base_branch": "main",
                    "branch": "ai-office/x",
                    "title": "タイトル",
                    "body": "本文",
                    "files": [{"path": "docs/a.md", "content": "c"}]
                }
            }
        }"#;
        let msg = ServerMsg::parse(json).unwrap();
        let ServerMsg::TaskAssign { task_id, payload } = msg else {
            panic!("expected TaskAssign");
        };
        assert_eq!(task_id, "t2");
        let payload: TaskAssignPayload = serde_json::from_value(payload).unwrap();
        assert_eq!(payload.mode, "remote");
        assert!(payload.actions.is_empty());
        let remote = payload.remote.expect("remote spec");
        assert_eq!(remote.repo, "owner/repo");
        assert_eq!(remote.base_branch, "main");
        assert_eq!(remote.branch, "ai-office/x");
        assert_eq!(remote.files.len(), 1);
        assert_eq!(remote.files[0].path, "docs/a.md");
        // token は省略されており、既定で空文字になる。
        assert_eq!(remote.token, "");
    }

    #[test]
    fn parse_task_cancel() {
        assert_eq!(
            ServerMsg::parse(r#"{"type":"task_cancel","task_id":"t1","reason":"不要"}"#).unwrap(),
            ServerMsg::TaskCancel {
                task_id: "t1".to_string(),
                reason: "不要".to_string(),
            }
        );
    }

    #[test]
    fn task_assign_payload_defaults() {
        let payload: TaskAssignPayload = serde_json::from_str(r#"{"mode":"local"}"#).unwrap();
        assert_eq!(payload.mode, "local");
        assert!(payload.actions.is_empty());
        assert!(payload.remote.is_none());
        assert_eq!(TaskAssignPayload::default().mode, "");
    }
}
