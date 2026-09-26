# ai-office

自宅鯖に住むAI社員の仮想オフィス。
サーバー（Go）が頭脳、クライアント（Rust）が手足。

## 構成
- server/ — Go製オーケストレーター
- client/ — Rust workspace
  - crates/protocol — 共通メッセージ型
  - crates/worker — 常駐ワーカー
  - crates/tui — 対話TUI
- secrets/ — APIキー等（git管理外）

## セットアップ
./setup.sh
