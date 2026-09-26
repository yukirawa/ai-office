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
            "ts": "2026-09-26T12:34:56Z"
        }"#;
        let msg = ServerMsg::parse(json).unwrap();
        match msg {
            ServerMsg::OfficeState {
                online,
                employees,
                ledger,
                relationships,
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
                ts,
            } => {
                assert!(online.is_empty());
                assert!(employees.is_empty());
                assert!(ledger.is_empty());
                assert!(relationships.is_empty());
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
}
