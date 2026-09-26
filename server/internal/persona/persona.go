// Package persona は AI 社員のキャラクター設定を扱う。
//
// 設計書 §6.1 の persona パッケージに対応する。Phase 1 では最小限の
// 構造体とローダのみを提供し、関係値（relationships）は store 側で扱う。
package persona

import (
	"encoding/json"
	"sort"
	"strings"
)

// Persona は AI 社員の人格設定。employees.persona_json に JSON で保存される。
type Persona struct {
	// Name は表示名。
	Name string `json:"name,omitempty"`
	// Role は役割の説明（"manager" などの employees.role とは別の、口語的な役割）。
	Role string `json:"role,omitempty"`
	// Gender は性別。未定の場合は空文字。
	Gender string `json:"gender,omitempty"`
	// Tone は口調の説明（例: "丁寧だが親しみやすい"）。
	Tone string `json:"tone,omitempty"`
	// Traits は性格特性のキーと値。
	Traits map[string]string `json:"traits,omitempty"`
	// System は LLM に渡すシステムプロンプトの上書き。空なら SystemPrompt が組み立てる。
	System string `json:"system,omitempty"`
}

// Load は persona_json 文字列をパースする。
// 空文字や不正な JSON の場合はゼロ値（空の Persona）を返し、エラーにはしない。
// 設定ミスでサーバが落ちないことを優先した設計判断。
func Load(raw string) Persona {
	var p Persona
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Persona{}
	}
	return p
}

// SystemPrompt は LLM に渡すシステムプロンプトを返す。
// System が設定されていればそれを優先し、無ければ Name/Role/Tone/Traits から組み立てる。
func (p Persona) SystemPrompt() string {
	if strings.TrimSpace(p.System) != "" {
		return p.System
	}
	var b strings.Builder
	if p.Name != "" {
		b.WriteString("あなたの名前は")
		b.WriteString(p.Name)
		b.WriteString("です。")
	}
	if p.Role != "" {
		b.WriteString("役割は「")
		b.WriteString(p.Role)
		b.WriteString("」です。")
	}
	if p.Tone != "" {
		b.WriteString("口調は")
		b.WriteString(p.Tone)
		b.WriteString("。")
	}
	if len(p.Traits) > 0 {
		keys := make([]string, 0, len(p.Traits))
		for k := range p.Traits {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+p.Traits[k])
		}
		b.WriteString("性格: ")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString("。")
	}
	return strings.TrimSpace(b.String())
}
