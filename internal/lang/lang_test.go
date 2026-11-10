package lang

import "testing"

func TestDetect_NamesTheLanguageOfRunningText(t *testing.T) {
	cases := []struct{ text, want string }{
		{"The licensee shall comply with the reporting duty and file it with the regulator by the end of the year.", "en"},
		{"Der Lizenznehmer muss die Meldepflicht erfüllen und sie bis zum Ende des Jahres bei der Behörde einreichen.", "de"},
		{"Le titulaire de la licence doit respecter l'obligation de déclaration et la déposer avant la fin de l'année.", "fr"},
		{"El titular de la licencia debe cumplir con la obligación de informar y presentarla antes del fin del año.", "es"},
		{"Ліцензіат має виконати обов'язок звітування та подати звіт до регулятора до кінця року.", "uk"},
		{"Лицензиат обязан выполнить обязанность отчётности и подать отчёт регулятору до конца года.", "ru"},
		{"Ο κάτοχος της άδειας πρέπει να υποβάλει την αναφορά έως το τέλος του έτους.", "el"},
		{"ライセンス保有者は年末までに報告書を提出しなければなりません。", "ja"},
		{"许可证持有人必须在年底前向监管机构提交报告。", "zh"},
		{"라이선스 보유자는 연말까지 보고서를 제출해야 합니다.", "ko"},
	}
	for _, c := range cases {
		if got := Detect(c.text); got != c.want {
			t.Errorf("Detect(%.40q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestDetect_TooLittleTextIsUnknown(t *testing.T) {
	for _, text := range []string{"", "RFC GE", "12345 67890", "Q3 plan"} {
		if got := Detect(text); got != Unknown {
			t.Errorf("Detect(%q) = %q, want unknown", text, got)
		}
	}
	if got := DetectOr("RFC GE", "en"); got != "en" {
		t.Errorf("DetectOr fallback = %q, want en", got)
	}
}

// An English text that names a Ukrainian city stays English: the majority script
// decides, not the presence of a letter.
func TestDetect_MajorityScriptDecides(t *testing.T) {
	if got := Detect("The office in Київ opened in May and it is the first one in the region of the country."); got != "en" {
		t.Errorf("got %q, want en", got)
	}
}
