package secdb

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// vectorFile 对应 testdata/vectors.json，由 scripts/gen_secdb_vectors.py 生成，
// 每条向量都已经过旧版 Python 实现解密校验。
type vectorFile struct {
	Keys struct {
		Trans string `json:"trans"`
		DB    string `json:"db"`
	} `json:"keys"`
	Cases []vectorCase `json:"cases"`
}

type vectorCase struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Input  string `json:"input"`
	Expect struct {
		Type      string   `json:"type"`
		DBIP      string   `json:"dbIp"`
		DBPort    int      `json:"dbPort"`
		Username  string   `json:"username"`
		Password  string   `json:"password"`
		DBName    string   `json:"dbname"`
		YxzIP     string   `json:"yxzIp"`
		SessionID string   `json:"sessionId"`
		ExtraKeys []string `json:"extraKeys"`
	} `json:"expect"`
}

func loadVectors(t *testing.T) (vectorFile, Key, Key) {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	trans, err := ParseKey(vf.Keys.Trans)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ParseKey(vf.Keys.DB)
	if err != nil {
		t.Fatal(err)
	}
	return vf, trans, db
}

// casesOf 返回指定格式的向量，并确保至少有一条，避免向量文件变动后测试空跑。
func casesOf(t *testing.T, vf vectorFile, format string) []vectorCase {
	t.Helper()
	var out []vectorCase
	for _, c := range vf.Cases {
		if c.Format == format {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("向量文件中没有 %s 格式的样本", format)
	}
	return out
}

func assertParams(t *testing.T, c vectorCase, got Params) {
	t.Helper()
	e := c.Expect
	if got.Type != e.Type || got.DBIP != e.DBIP || got.DBPort != e.DBPort ||
		got.Username != e.Username || got.Password != e.Password || got.DBName != e.DBName ||
		got.YxzIP != e.YxzIP || got.SessionID != e.SessionID {
		t.Errorf("字段与旧实现不一致:\n got  type=%q dbIp=%q dbPort=%d user=%q pw=%q dbname=%q yxz=%q sess=%q\n want type=%q dbIp=%q dbPort=%d user=%q pw=%q dbname=%q yxz=%q sess=%q",
			got.Type, got.DBIP, got.DBPort, got.Username, got.Password, got.DBName, got.YxzIP, got.SessionID,
			e.Type, e.DBIP, e.DBPort, e.Username, e.Password, e.DBName, e.YxzIP, e.SessionID)
	}
	keys := make([]string, 0, len(got.Extra))
	for k := range got.Extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := e.ExtraKeys
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(keys, want) {
		t.Errorf("Extra 字段名 = %v, want %v", keys, want)
	}
}

func TestDecodeDBFieldMatchesLegacyVectors(t *testing.T) {
	vf, _, dbKey := loadVectors(t)
	for _, c := range casesOf(t, vf, "dbfield") {
		t.Run(c.Name, func(t *testing.T) {
			got, err := DecodeDBField(dbKey, c.Input)
			if err != nil {
				t.Fatalf("DecodeDBField: %v", err)
			}
			assertParams(t, c, got)
		})
	}
}

func TestDecodeSSOSecDBMatchesLegacyVectors(t *testing.T) {
	vf, transKey, dbKey := loadVectors(t)
	for _, c := range casesOf(t, vf, "ssosecdb") {
		t.Run(c.Name, func(t *testing.T) {
			got, err := DecodeSSOSecDB(transKey, dbKey, c.Input)
			if err != nil {
				t.Fatalf("DecodeSSOSecDB: %v", err)
			}
			assertParams(t, c, got)
		})
	}
}
