package schema

import "testing"

func TestPhysicalSQLTokenNormalization(t *testing.T) {
	base := "CREATE TABLE `notes` (id TEXT DEFAULT (lower(hex(randomblob(7)))), title TEXT DEFAULT 'Keep  Case')"
	formatted := "create  table `notes`(id text default(lower (hex (randomblob (7)))), title text default 'Keep  Case') -- comment\n"
	if Hash(sqlTokens(base)) != Hash(sqlTokens(formatted)) {
		t.Fatal("formatter changes altered fingerprint")
	}
	for _, changed := range []string{
		"CREATE TABLE `notes` (id TEXT DEFAULT (lower(hex(randomblob(7)))), title TEXT DEFAULT 'keep  case')",
		"CREATE TABLE `notes` (id TEXT DEFAULT (lower(hex(randomblob(7)))), title TEXT DEFAULT 'Keep Case')",
		"CREATE TABLE `notes` (id TEXT DEFAULT (lower(hex(randomblob(8)))), title TEXT DEFAULT 'Keep  Case')",
		"CREATE TABLE `notes` (id TEXT DEFAULT (lower(hex(randomblob(7)))), title TEXT DEFAULT 'Keep  Case', extra TEXT)",
	} {
		if Hash(sqlTokens(base)) == Hash(sqlTokens(changed)) {
			t.Fatalf("semantic SQL change hidden: %s", changed)
		}
	}
	for _, pair := range [][2]string{
		{"SELECT 'a''--b/*c*/'", "select 'a''--b/*c*/'"},
		{"SELECT \"a b\", `c``d`, [e f]", "select /*comment*/ \"a b\", `c``d`, [e f]"},
	} {
		if Hash(sqlTokens(pair[0])) != Hash(sqlTokens(pair[1])) {
			t.Fatal("quoted token was split")
		}
	}
	if Hash(sqlTokens("SELECT ab c")) == Hash(sqlTokens("SELECT a bc")) {
		t.Fatal("token boundaries lost")
	}
}
