package receipt

import (
	"bytes"
	"fmt"
	"html/template"
	"time"

	"github.com/swibit/flowed/internal/domain/money"
)

// RenderHTML produces a self-contained printable receipt.
//
// Self-contained matters more than it sounds: a cashier's desk may be on a
// machine with no internet, and a receipt that silently loses its stylesheet
// prints as a column of unformatted text that no finance office will accept.
// Everything — layout, fonts fallback, the logo if there is one — is inline.
//
// The page is sized A5, which is what university finance offices in Iraq
// generally stock, and the print stylesheet drops the browser's own headers so
// the paper carries only the document.
func RenderHTML(d Data, loc *time.Location) ([]byte, error) {
	var buf bytes.Buffer
	if err := htmlTemplate.Execute(&buf, newView(d, loc)); err != nil {
		return nil, fmt.Errorf("rendering receipt %s: %w", d.Number, err)
	}
	return buf.Bytes(), nil
}

// view is Data with everything already formatted, so the template contains no
// logic beyond iteration and a few conditionals. Formatting in Go rather than
// in the template keeps the number formatting identical between the HTML and
// plain-text renderers.
type view struct {
	Data

	IssuedAtText  string
	PrintedAtText string
	AmountText    string
	TotalFeesText string
	DiscountText  string
	NetFeesText   string
	PaidText      string
	RemainingText string
	VoidedAtText  string
	Lines         []lineView
	ShowSummary   bool
}

type lineView struct {
	Label      string
	DueText    string
	AmountText string
}

func newView(d Data, loc *time.Location) view {
	v := view{
		Data:          d,
		IssuedAtText:  FormatDateTime(d.IssuedAt, loc),
		PrintedAtText: FormatDateTime(d.PrintedAt, loc),
		AmountText:    money.FormatWesternDigits(d.Amount),
		TotalFeesText: money.FormatWesternDigits(d.TotalFees),
		DiscountText:  money.FormatWesternDigits(d.TotalDiscount),
		NetFeesText:   money.FormatWesternDigits(d.NetFees),
		PaidText:      money.FormatWesternDigits(d.PaidToDate),
		RemainingText: money.FormatWesternDigits(d.Remaining),
		// A refund carries no account summary: the figures on it describe the
		// payment being reversed, and printing a balance beside them invites
		// the two to be read as one.
		ShowSummary: d.Kind == KindPayment && d.NetFees.IsPositive(),
	}
	if d.VoidedAt != nil {
		v.VoidedAtText = FormatDateTime(*d.VoidedAt, loc)
	}
	for _, l := range d.Lines {
		lv := lineView{Label: l.Label, AmountText: money.FormatWesternDigits(l.Amount)}
		if l.DueDate != nil {
			lv.DueText = FormatDate(*l.DueDate)
		}
		v.Lines = append(v.Lines, lv)
	}
	return v
}

var htmlTemplate = template.Must(template.New("receipt").Parse(receiptHTML))

// The document is right-to-left throughout, but every figure is in Western
// digits. Arabic-Indic ٠ and Western 0 are easy to confuse on a poorly printed
// slip, and a figure that can be misread is a figure that can be disputed.
const receiptHTML = `<!doctype html>
<html lang="ar" dir="rtl">
<head>
<meta charset="utf-8">
<title>{{.Title}} {{.Number}}</title>
<style>
  @page { size: A5; margin: 10mm; }
  * { box-sizing: border-box; }
  body {
    font-family: "Segoe UI", Tahoma, "Noto Naskh Arabic", "Arabic Typesetting", sans-serif;
    font-size: 12pt; line-height: 1.6; color: #000; background: #fff;
    margin: 0; padding: 8mm;
  }
  .sheet { max-width: 148mm; margin: 0 auto; position: relative; }

  header { text-align: center; border-bottom: 2px solid #000; padding-bottom: 6px; }
  .university { font-size: 15pt; font-weight: 700; }
  .college { font-size: 12pt; }
  .contact { font-size: 9pt; color: #444; }
  .logo { max-height: 18mm; margin-bottom: 4px; }

  .title-row {
    display: flex; justify-content: space-between; align-items: baseline;
    margin: 10px 0 6px;
  }
  .doc-title { font-size: 16pt; font-weight: 700; }
  .doc-number { font-size: 13pt; font-weight: 700; letter-spacing: 0.5px; }
  .doc-number .label { font-size: 9pt; font-weight: 400; color: #444; }

  table { width: 100%; border-collapse: collapse; }
  .fields td { padding: 3px 4px; vertical-align: top; }
  .fields td.k { color: #444; width: 26%; font-size: 10pt; }
  .fields td.v { font-weight: 600; }

  .amount-box {
    border: 2px solid #000; padding: 8px 10px; margin: 10px 0;
  }
  .amount-figures { display: flex; justify-content: space-between; align-items: baseline; }
  .amount-figures .label { font-size: 10pt; color: #444; }
  .amount-figures .value { font-size: 18pt; font-weight: 700; }
  .amount-words {
    margin-top: 6px; padding-top: 6px; border-top: 1px dashed #666;
    font-size: 11pt;
  }
  .amount-words .label { font-size: 9pt; color: #444; display: block; }

  .lines { margin-top: 8px; }
  .lines th, .lines td {
    border: 1px solid #999; padding: 4px 6px; font-size: 10pt; text-align: right;
  }
  .lines th { background: #f0f0f0; font-weight: 600; }
  .lines td.num { text-align: left; font-variant-numeric: tabular-nums; }

  .summary { margin-top: 8px; }
  .summary td { padding: 2px 6px; font-size: 10pt; }
  .summary td.num { text-align: left; font-variant-numeric: tabular-nums; font-weight: 600; }
  .summary tr.total td { border-top: 1px solid #000; font-size: 11pt; font-weight: 700; }

  .signatures { margin-top: 14mm; display: flex; justify-content: space-between; }
  .sig { width: 45%; text-align: center; font-size: 10pt; }
  .sig .line { border-top: 1px solid #000; margin-top: 12mm; padding-top: 3px; }

  footer { margin-top: 6px; font-size: 8pt; color: #555; text-align: center; }

  /* A reprint says so across the face of the document. Two papers each
     claiming to be the original receipt for one payment is exactly the
     ambiguity a forger needs. */
  .stamp {
    position: absolute; top: 38%; right: 50%; transform: translateX(50%) rotate(-18deg);
    font-size: 34pt; font-weight: 700; letter-spacing: 3px;
    color: rgba(180, 0, 0, 0.16); border: 4px solid rgba(180, 0, 0, 0.16);
    padding: 6px 22px; white-space: nowrap; pointer-events: none;
  }
  .voided .amount-figures .value,
  .voided .doc-number { text-decoration: line-through; }
  .void-notice {
    border: 2px solid #b00; color: #b00; padding: 5px 8px; margin: 8px 0;
    font-weight: 700; font-size: 11pt;
  }

  @media print {
    body { padding: 0; }
    .no-print { display: none; }
  }
</style>
</head>
<body class="{{if .IsVoided}}voided{{end}}">
<div class="sheet">

  {{if .IsCopy}}<div class="stamp">{{.CopyLabel}}</div>{{end}}

  <header>
    {{if .Institution.LogoDataURI}}<img class="logo" src="{{.Institution.LogoDataURI}}" alt="">{{end}}
    <div class="university">{{.Institution.UniversityNameAr}}</div>
    {{if .Institution.CollegeNameAr}}<div class="college">{{.Institution.CollegeNameAr}}</div>{{end}}
    {{if or .Institution.Address .Institution.Phone}}
      <div class="contact">{{.Institution.Address}}{{if and .Institution.Address .Institution.Phone}} — {{end}}{{.Institution.Phone}}</div>
    {{end}}
  </header>

  <div class="title-row">
    <div class="doc-title">{{.Title}}</div>
    <div class="doc-number"><span class="label">الرقم</span> {{.Number}}</div>
  </div>

  {{if .IsVoided}}
    <div class="void-notice">
      سند ملغى بتاريخ {{.VoidedAtText}}{{if .VoidReason}} — {{.VoidReason}}{{end}}
    </div>
  {{end}}

  <table class="fields">
    <tr>
      <td class="k">التاريخ</td><td class="v">{{.IssuedAtText}}</td>
      <td class="k">العام الدراسي</td><td class="v">{{.AcademicYear}}</td>
    </tr>
    <tr>
      <td class="k">اسم الطالب</td><td class="v">{{.StudentName}}</td>
      <td class="k">الرقم الجامعي</td><td class="v">{{.StudentNumber}}</td>
    </tr>
    <tr>
      <td class="k">اسم الأم</td><td class="v">{{.MotherName}}</td>
      <td class="k">الكلية</td><td class="v">{{.CollegeName}}</td>
    </tr>
    <tr>
      <td class="k">القسم</td><td class="v">{{.DepartmentName}}</td>
      <td class="k">المرحلة / الدراسة</td><td class="v">{{.StageLabel}} — {{.StudyTypeName}}</td>
    </tr>
    {{if .PayerName}}
    <tr><td class="k">المسلِّم</td><td class="v" colspan="3">{{.PayerName}}</td></tr>
    {{end}}
  </table>

  <div class="amount-box">
    <div class="amount-figures">
      <span class="label">{{.AmountLabel}}</span>
      <span class="value">{{.AmountText}} د.ع</span>
    </div>
    <div class="amount-words">
      <span class="label">المبلغ كتابةً</span>
      {{.AmountInWords}}
    </div>
  </div>

  <table class="fields">
    <tr>
      <td class="k">طريقة الدفع</td><td class="v">{{.PaymentMethod}}</td>
      {{if .MethodReference}}<td class="k">رقم الإشعار</td><td class="v">{{.MethodReference}}</td>{{else}}<td></td><td></td>{{end}}
    </tr>
  </table>

  {{if .Lines}}
  <table class="lines">
    <thead><tr><th>البيان</th><th>تاريخ الاستحقاق</th><th>المبلغ</th></tr></thead>
    <tbody>
      {{range .Lines}}
      <tr>
        <td>{{.Label}}</td>
        <td>{{if .DueText}}{{.DueText}}{{else}}—{{end}}</td>
        <td class="num">{{.AmountText}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{end}}

  {{if .ShowSummary}}
  <table class="summary">
    <tr><td>مجموع الرسوم</td><td class="num">{{.TotalFeesText}}</td></tr>
    <tr><td>الخصومات</td><td class="num">{{.DiscountText}}</td></tr>
    <tr><td>الصافي</td><td class="num">{{.NetFeesText}}</td></tr>
    <tr><td>المسدَّد لغاية تاريخه</td><td class="num">{{.PaidText}}</td></tr>
    <tr class="total"><td>المتبقي</td><td class="num">{{.RemainingText}}</td></tr>
  </table>
  {{end}}

  {{if .Notes}}<p style="font-size:10pt;margin-top:6px;">ملاحظات: {{.Notes}}</p>{{end}}

  <div class="signatures">
    <div class="sig">{{.CashierName}}<div class="line">توقيع أمين الصندوق</div></div>
    <div class="sig">&nbsp;<div class="line">توقيع المستلم</div></div>
  </div>

  <footer>
    طُبع في {{.PrintedAtText}}{{if .PrintedBy}} بواسطة {{.PrintedBy}}{{end}}
    {{if .IsCopy}} — {{.CopyLabel}}{{end}}
  </footer>
</div>
</body>
</html>
`
