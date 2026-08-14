# نظام إدارة أجور وأقساط الطلبة — جامعة عراقية
## Domain Model & Architecture — الوثيقة التصميمية

> منهجية الإعداد: 7 تصاميم متخصصة متوازية (أكاديمي، مالي، خصومات، قاعدة بيانات، تقارير، Frappe، أوامر Domain) خضعت لثلاث مراجعات عدائية (تدقيق مالي، تدقيق سلامة تاريخية، تدقيق واقعية Frappe). كل قرار في هذه الوثيقة إما نجا من المراجعة أو عُدّل بسببها. لا يوجد implementation code — Domain أولاً.

---

## 1. Executive Summary

**حكمي على فكرتك الأساسية: صحيحة، وهي حجر الأساس الوحيد الذي لا يجوز التنازل عنه.** الطالب كيان هوية ثابت، وكل سنة دراسية تُمثَّل بـ Enrollment مستقل يحمل سياقه الأكاديمي وحسابه المالي. البديل (حقول stage/study_type على الطالب مباشرة) يؤدي حتماً إلى فقدان التاريخ، واستحالة احتساب رسوم المعيد، واستحالة تقارير السنوات السابقة. التصميم أدناه يبني على فكرتك مع تعديلين جوهريين وعدة تحصينات.

**المبادئ الخمسة الحاكمة:**

1. **Immutability**: كل وثيقة مالية (Payment, Refund, Snapshot, DiscountApplication) لا تُعدَّل ولا تُحذف بعد ترحيلها. التصحيح يكون بوثيقة معاكسة (Void / Refund / Adjustment) — النظام append-only في كل ما يخص المال.
2. **Configuration versioned + Snapshot**: الرسوم والخصومات configuration مؤرَّخة بإصدارات immutable، وتُجمَّد نسخة منها (snapshot) داخل الحساب المالي لحظة إنشائه. تغيير الإعدادات لاحقاً **لا يستطيع بنيوياً** تغيير حسابات قديمة — لأن القديم لا يشير إلى "القيمة الحالية" أصلاً.
3. **ثلاثة أبعاد حالة منفصلة**: `academic_result` و `enrollment_status` و حالة الحساب المالي — لا تُدمج أبداً في حقل واحد.
4. **Recomputability**: الرصيد الحقيقي يُشتق دوماً من الوثائق. الـ totals المخزّنة كاش يُحدَّث **داخل نفس الـ transaction وتحت قفل الحساب**، مع مصالحة ليلية (كشف الانحراف = تقرير فارغ عند الصحة).
5. **Domain Commands لا CRUD**: كل تغيير مالي عبر أمر مسمّى (RecordPayment, VoidPayment…) له صلاحيات وشروط مسبقة وidempotency وأثر تدقيقي.

**تعديلان جوهريان على افتراضاتك (النقد الصريح الذي طلبته):**

- **إغلاق السنة بحالة واحدة `Closed` خطأ في السياق العراقي.** نتائج الدور الثاني تصل بعد أن تريد المالية غلق دفاترها. الصحيح: إغلاق على مرحلتين — `FinanciallyClosed` (يقفل المال) ثم `Closed` (يقفل الأكاديمي). ويجب السماح بسنتين مفتوحتين بآن واحد (فترة التداخل بين الدور الثاني وتسجيل السنة الجديدة).
- **الاستضافة ليست قيمة في Study Type.** الطالب المستضاف له نوعا دراسة (أصلي وفعلي) وجهتان — قيمة واحدة `HOSTING` تدمّر هذا الزوج وتفسد FeePolicy. الصحيح: الاستضافة **overlay** على الـ Enrollment (سجل HostingRecord بمصدر وهدف واتجاه).

**منصة التنفيذ:** تطبيق Frappe مخصص (`uni_fees`) — **لا** تستخدم ERPNext Education (منفصل عن النواة، صيانته ضعيفة، ونموذجه غربي لا يعرف المرحلة/المعيد/نوع الدراسة).

---

## 2. Core Domain Concepts

الكيانات مصنّفة بخمس فئات — التصنيف نفسه قاعدة معمارية:

| فئة | كيانات | خصائصها |
|---|---|---|
| **Identity** | Student | ثابتة، لا تحمل شيئاً أكاديمياً أو مالياً |
| **Context** | Enrollment, AcademicYear, College, Department, StudyType, HostingRecord | سياق سنوي؛ لا يُعدَّل تاريخياً بل يُستبدل (supersede) |
| **Configuration** | FeePolicyVersion, FeeComponent, DiscountDefinitionVersion, InstallmentTemplate | versioned و immutable بعد النشر؛ تغييرها = إصدار جديد |
| **Transaction** | FinancialAccount (+FeeSnapshotLine), DiscountApplication, Installment, Payment (+PaymentAllocation), Refund (+RefundAllocation), AccountAdjustment, CreditEntry, CashierSession | وثائق مالية append-only |
| **Projection** | كاش الـ totals، الـ views، التقارير | مشتقة، قابلة لإعادة البناء من الـ Transactions |

**الفرق بين Student و Enrollment** (سؤالك المباشر): Student يجيب "من هذا الشخص؟" فقط. Enrollment يجيب "بأي صفة درس هذا الشخص في السنة X؟". القاعدة الحاسمة: **أي حقل يمكن أن تختلف قيمته بين سنتين دراسيتين، وجوده على Student غير قانوني.**

---

## 3. Entity Model

### 3.1 Student

| مجموعة | حقول | سياسة التغيير |
|---|---|---|
| هوية ثابتة | `student_id` (surrogate PK)، `student_no` (رقم جامعي unique، لا يُعاد استخدامه أبداً)، `first_admission_year` | لا تتغير إطلاقاً. دمج التكرارات بـ tombstone (`merged_into`)، لا حذف |
| قابلة للتعديل (غير تاريخية) | هاتف 1/2، بريد، عنوان حالي، ولي الأمر، صورة، ملاحظات | تعديل مباشر + audit log |
| هوية موثّقة **versioned** | الاسم الرباعي، اسم الأم، المواليد، الرقم الوطني، الجنس، الجنسية | **StudentIdentityVersion** append-only: المحاكم العراقية تغيّر الأسماء والقيود؛ كل نسخة تحمل رقم قرار المحكمة وتاريخه. الوثائق الصادرة قبل التغيير تشير إلى النسخة النافذة وقتها. على Student نسخة denormalized حالية **للبحث فقط** |
| بحث | `full_name_norm`, `mother_name_norm`, `phone_norm`, `phone_reversed` | تُملأ آلياً عند كل كتابة (تطبيع عربي — §15) |

### 3.2 AcademicYear

حقول: `code` (2025-2026, unique)، تواريخ البداية/النهاية، `status`، `registration_deadline`، سياسة حجب التسجيل بالدين (per-year)، مرجع سلاسل الوصولات، أختام الإغلاق (من/متى لكل مرحلة).

**دورة الحياة (المعدَّلة):** `Draft → Open → FinanciallyClosed → Closed` — بلا رجوع أبداً.

| المرحلة | ماذا تقفل | ماذا تُبقي مفتوحاً |
|---|---|---|
| `FinanciallyClosed` | إنشاء حسابات مالية في السنة؛ تعديل snapshots/خطط أقساط/خصومات السنة؛ **Void** لدفعات سلسلة السنة؛ Refund لدفعات السنة (يمرّ حصراً عبر Adjustment) | إدخال نتائج الدور الثاني، تثبيت الحالات الأكاديمية، التخرج |
| `Closed` | كل شيء. شرط الدخول: كل enrollment غير منتهٍ له نتيجة مسجلة أو `NoResult` صريح | القراءة فقط (التقارير لا تتأثر بالإغلاق أبداً) |
| `AdjustmentOpen` | حالة استثنائية مؤقتة time-boxed بقرار مزدوج (Admin + إشعار Auditor آلي)، تسمح بقائمة أوامر محددة فقط (Adjustment/Refund موثَّق)، ثم تُغلق آلياً | — |

**قاعدة حاسمة (صحّحها المدقق المالي):** تحصيل ديون سنة مغلقة **عملية اعتيادية مشروعة** — الدفعة الجديدة تُرحَّل بتاريخ اليوم وبسلسلة وصولات السنة المفتوحة الحالية، وتُخصَّص على أقساط حساب السنة القديمة. الإغلاق يمنع الترحيل *المؤرَّخ إلى* السنة المغلقة وتعديل بياناتها، ولا يمنع استيفاء ديونها. بدون هذه القاعدة سيتحايل الصرافون على النظام من اليوم الأول.

### 3.3 Enrollment — الوحدة الأكاديمية والمالية

| مجموعة | حقول |
|---|---|
| هوية | `enrollment_id`, `student`, `academic_year`, `sequence_no` |
| سياق | `college`, `department`, `stage` (1..6 بحسب سنوات القسم), `study_type`, `student_category` (Regular/Repeat/… — مشتق يُغذي FeePolicy), `attempt_number` |
| نوع | `enrollment_kind`: `Regular` \| `HostedIn` \| `TransferIn` |
| حالة | `enrollment_status`, `academic_result`, `result_decision_flag` (ناجح بقرار) |
| سلسلة تاريخية | `previous_enrollment` (تسجيل السنة السابقة), `supersedes`, `superseded_by`, `supersede_reason` |
| وثائق | مراجع أوامر التأجيل/النقل/العودة، `registered_at/by` |

**قواعد الفرادة:**
- unique `(student, academic_year, sequence_no)` — سلسلة supersede محددة.
- **invariant إضافي (سدّ ثغرة):** enrollment حيّ واحد كحد أقصى لكل (طالب، سنة) — عبر عمود `is_current` (قيمته 1 أو NULL) مع unique `(student, year, is_current)`؛ بدونه تسجيلان متزامنان = سياقان ماليان حيّان.
- `attempt_number`: عدد المحاولات **المحتسبة** لنفس (القسم، المرحلة). قاعدة الاحتساب مشتقة من النتيجة حصراً: `Failed` تُحتسب، `NoResult/NotApplicable` لا تُحتسب (قرار حسم غموض "انقطع قبل الامتحانات"). يُخزَّن لكن **يُتحقق منه آلياً ضد السلسلة عند الإنشاء** — ليس إدخالاً حراً.

**نمط Supersede (تغيير قسم/نوع دراسة منتصف السنة):**
1. أمر `SupersedeEnrollment(old, changes, reason, approval)` — بشرط سنة Open وحالة Active وموافقة.
2. إنشاء Enrollment جديد بـ `sequence_no+1` والحقل المتغيّر، مع `supersedes` → القديم.
3. القديم: `status=Superseded`, `academic_result=NotApplicable` (النتيجة الحقيقية على الخلَف).
4. **الحساب المالي القديم يُغلق مجمَّداً (close-and-open)، لا تُنقل الدفعات أبداً** — الوصولات المطبوعة تشير إليه ونقل allocations = تزوير تاريخ. يُرحَّل الرصيد بزوج **TransferCredit** (قيد Adjustment موقَّع ظاهر): يُسوّي القديم ويفتح الجديد برصيد دائن = المدفوع، ثم snapshot جديد من FeePolicy السياق الجديد وخطة أقساط جديدة للمتبقي.
5. Refund على دفعة تعود لحساب Superseded **محظور** (يفسد الـ TransferCredit) — التصحيح عبر adjustments الحساب الخلَف.

**تمديد ضروري (كشفه مدقق التاريخ):** وصول نتيجة دور ثانٍ متأخرة *بعد* أن سُجّل الطالب معيداً في السنة الجديدة ودفع رسم المعيد ⇒ supersede بسبب `LateR2Result` يسمح بتصحيح stage/category/attempt، مع Adjustment إلزامي لفرق رسم المعيد. هذه ليست "policy choice" — تحدث كل خريف.

### 3.4 StudyType والاستضافة

`StudyType` = master data قابل للإضافة إدارياً: `code`, `name_ar`, `is_active` فقط. Morning/Evening/Parallel صفوف بيانات، وأي نوع مستقبلي صف جديد بلا كود.

**الاستضافة = HostingRecord (1:0..1 مع Enrollment):** `direction` (Incoming/Outgoing)، جامعة/كلية/قسم/نوع دراسة **الأم**، وجامعة/كلية/قسم/نوع دراسة **المضيف** (الجهات الخارجية نص حر)، فترة، رقم كتاب الاستضافة.
- **Incoming** (طالبهم عندنا): Student (إن لم يوجد) + Enrollment بـ `kind=HostedIn` و `study_type` = النوع الذي يداوم به **عندنا** (هو ما يغذي أي رسوم محلية)؛ سياق جهته الأم في السجل.
- **Outgoing** (طالبنا عندهم): تسجيله الأصلي **يبقى Active** (يظل طالبنا وتعود نتائجه إلينا) + HostingRecord بالاتجاه الصادر. لا supersede.
- **علم سياسة غير محسوم عمداً**: من يقبض رسوم المستضاف (الجامعة الأم / المضيفة / تقاسم)؟ يُهيّأ **لكل اتفاقية استضافة** على السجل نفسه — ليس قاعدة ثابتة في الكود.

### 3.5 بقية الكيانات

تُفصَّل في أقسامها: FeePolicy (§6)، الخصومات (§7)، الأقساط (§8)، الدفع (§9)، الاسترجاع/الإلغاء (§10)، والكيانان اللذان أضافتهما المراجعة العدائية:
- **AccountAdjustment** — قيد تسوية موقَّع append-only على الحساب: أنواعه `TransferCreditOut/In` (زوج supersede)، `ClosedYearAdjustment`، `Waiver`، `LateR2Delta`، `DiscountRetro`. يحمل مبلغاً موجباً/سالباً، سبباً، موافقة، ومرجع الوثيقة المقابلة. **يدخل في معادلة الرصيد**: `effective_net = net_snapshot + Σ adjustments`.
- **CreditEntry** — رصيد دائن للطالب (دفع زائد، خصم رجعي): صف قابل للقفل (`FOR UPDATE`) حالته Open/Consumed/Refunded — يمنع صرف الرصيد مرتين (ثغرة اكتُشفت: تعويض آلي في حساب السنة الجديدة + refund نقدي متزامنان لنفس الرصيد).

---

## 4. Relationships

| علاقة | نوع |
|---|---|
| Student → Enrollment | 1:N |
| Student → StudentIdentityVersion / DiscountAssignment | 1:N |
| AcademicYear → Enrollment / FeePolicyVersion / NumberSeries | 1:N |
| College → Department؛ Department → Enrollment | 1:N |
| Enrollment → HostingRecord | 1:0..1 |
| Enrollment → Enrollment (supersede) | سلسلة 1:0..1 (unique على `superseded_by`) |
| Enrollment → Enrollment (`previous_enrollment` عبر السنوات) | N:1 |
| **Enrollment → FinancialAccount** | **1:1** (قاعدة النظام المركزية) |
| FinancialAccount → FeeSnapshotLine / Installment / DiscountApplication / AccountAdjustment / CreditEntry / Payment | 1:N |
| FeePolicyVersion → FeeComponent | 1:N |
| DiscountDefinition → DiscountDefinitionVersion → DiscountApplication | 1:N، 1:N |
| DiscountAssignment → DiscountApplication | 1:N (تطبيق لكل حساب سنة) |
| **Payment ↔ Installment** | **N:M عبر PaymentAllocation** |
| **Refund ↔ Installment** | **N:M عبر RefundAllocation** (مرآة الدفع — إضافة المراجعة) |
| Payment → Refund | 1:N |
| CashierSession → Payment | 1:N |
| ImportBatch → ImportRow | 1:N |

**قرار v1:** الدفعة الواحدة تخص **حساباً مالياً واحداً**. طالب يسدّد دين سنة سابقة + قسط السنة الحالية = وصلان. (دفعة cross-account تكسر نموذج الأقفال وسلاسل الوصولات — مؤجلة كـ extension بشروط صارمة، §22).

---

## 5. Academic Lifecycle

| حدث | التمثيل |
|---|---|
| نجاح وترقية | نتيجة `Passed_R1/R2` على تسجيل السنة؛ السنة التالية Enrollment جديد stage+1، attempt=1، مربوط بـ `previous_enrollment` |
| رسوب وإعادة | `Failed`؛ السنة التالية نفس المرحلة، attempt+1، `student_category=Repeat` (محرك رسم المعيد) |
| إعادة متكررة | attempt=3، 4… — لا سقف في النموذج؛ سقوف الوزارة فحص سياسة قابل للتهيئة |
| دور أول/ثاني | `Passed_R1` مقابل `Passed_R2`؛ بين الدورين النتيجة `Pending` والحالة Active. ناجح بقرار = `Passed_* + result_decision_flag` |
| تأجيل | `Deferred` + `NotApplicable` + مرجع أمر التأجيل. لا تُحتسب محاولةً. المعالجة المالية (إبقاء/إسقاط الرسوم) بارامتر صريح في الأمر — لا سلوك ضمني |
| انقطاع | `DroppedOut` + `NoResult`؛ السنوات اللاحقة ببساطة بلا صفوف. **الدين يبقى على حسابه** |
| عودة بعد انقطاع | Enrollment جديد بسنة العودة مربوط بآخر تسجيل محتسب؛ فحص سياسة حجب الدين عند التفعيل |
| نقل خارج | `TransferredOut` + مرجع الأمر؛ الحساب يُسوّى/يُجمَّد؛ Student → TransferredOut |
| نقل داخل | Enrollment جديد `TransferIn` (منتصف السنة) أو Regular (بين السنوات) |
| انسحاب | `Withdrawn` + `NoResult` |
| تخرج | تسجيل المرحلة الأخيرة: نتيجة `Passed_*` وحالة `Completed`؛ حالة الطالب Graduated **مشتقة** من التسجيل، ليست علماً حراً. براءة الذمة شرط قابل للتهيئة (block/flag) |

**مصفوفة شرعية الحالات** (أي توليفة خارجها تُرفض عند الكتابة):

| enrollment_status | academic_result المسموحة |
|---|---|
| Draft | Pending |
| Active | Pending, Passed_R1, Passed_R2, Failed |
| Deferred | NotApplicable |
| Superseded | NotApplicable |
| TransferredOut | NoResult, NotApplicable |
| Withdrawn | NoResult |
| DroppedOut | NoResult, Failed |
| Completed | Passed_R1, Passed_R2 |

---

## 6. Financial Lifecycle

### 6.1 FeePolicy (الرسوم كإعدادات — ممنوع الـ if في الكود)

**FeePolicyVersion** (immutable بعد النشر؛ التعديل = إصدار جديد): أبعاد `(academic_year, college, department, stage, study_type, student_category)` — البعد الفارغ = wildcard لكن يُخزَّن **بقيمة sentinel `ALL` وليس NULL** (unique index على NULL لا يعمل في MariaDB — ثغرة اكتُشفت وأُغلقت). حالة Draft/Published/Retired.

**FeeComponent** (child): `code` (TUITION, REGISTRATION, ID_CARD, LAB…), `name_ar`, `amount` IQD, **`discountable`** (هوية الطالب مثلاً غير قابلة للخصم), `refundable`, `mandatory`. **نعم — أكثر من Fee Component مدعوم من اليوم الأول**، وهو ما يجعل الخصومات والإعفاءات تعمل صح.

**Resolution (خوارزمية واحدة حصراً — كان هناك تعارض بين وثيقتين وحُسم):** بين الصفوف المنشورة المطابقة، الفائز صاحب أعلى مجموع أوزان قوى الاثنين: `year=32, college=16, department=8, stage=4, study_type=2, category=1`. أوزان قوى الاثنين تجعل التعادل بين مجموعات مختلفة مستحيلاً رياضياً. لا مطابقة = **خطأ صريح، لا يُنشأ الحساب** — لا default صفري أبداً. الـ score الفائز يُخزَّن في الـ snapshot للتدقيق.

**سؤالك "هل الرسوم snapshot؟" — نعم، قطعياً.** الـ resolution يجري مرة واحدة عند `GenerateFinancialAccount` وتُجمَّد النتيجة سطراً سطراً (`FeeSnapshotLine`). تغيّرت الإعدادات بعدها؟ الحسابات القائمة لا تتأثر بنيوياً.

### 6.2 FinancialAccount

- يُنشأ بأمر صريح (فردي أو bulk §18) — ليس side effect للتسجيل.
- **Snapshot المجمَّد**: مرجع policy+version+score، السطور بالمبالغ، `gross_total`، تطبيقات الخصم المادية (كلٌّ بمرجع إصدار التعريف ومبلغ مجمَّد)، `discount_total`، `net_total`.
- **المعادلة بعد المراجعة** (حلّت تناقض "المجمَّد الذي يتغير"): `net_total` مجمَّد للأبد؛ كل تغيير لاحق (خصم رجعي، supersede، تسوية) قيد `AccountAdjustment` منفصل؛ **`effective_net = net_total + Σ adjustments`** وهو مرجع الأقساط والرصيد.
- **derived مقابل cached** (سؤالك المباشر): المجاميع المجمَّدة (gross/discount/net) snapshot وليست كاشاً. أما `paid_total, refunded_total, credit_balance, balance_due` فكاش يُعاد حسابه **بشكل متزامن داخل كل transaction مالية تحت قفل صف الحساب**، + مهمة ليلية تصالح الكاش مع إعادة الاحتساب الكامل (أي انحراف = خلل يُصحَّح ويُسجَّل، ومعدله الطبيعي صفر). القاعدة الذهبية للتقارير: **كل رقم يُطبع أو يُدقَّق قانونياً يُقرأ من الصفوف الخام؛ لوحات العدّ والملخصات يجوز أن تقرأ الكاش** (§16).
- حالات: `Pending → Active ⇄ Settled`؛ `Cancelled` (supersede/انسحاب بعد التسوية). تجميد السنة المغلقة guard متعامد، ليس حالة.
- فرادة 1:1 مع Enrollment عبر عمود `enrollment_active` (flag-trick) كي لا يمنع حسابٌ ملغى إعادة التوليد.

### 6.3 دين السنوات السابقة

**الدين لا يُرحَّل ولا يُنقل — يبقى على حساب سنته.** "إجمالي ذمة الطالب" = تجميع قراءةً عبر حساباته. حجب تسجيل السنة الجديدة بالدين سياسة قابلة للتهيئة لكل سنة (block / warn / ignore) تُفحص عند تفعيل التسجيل. التحصيل بعد إغلاق السنة مشروع كما في §3.2.

---

## 7. Discount Model

### 7.1 الطبقات الثلاث (إجابة سؤالك Definition/Assignment/Application)

| طبقة | ماهيتها | حقول جوهرية |
|---|---|---|
| **DiscountDefinition** + **Version** | *ما هو الخصم؟* — Header ثابت (code, name_ar, category, exclusivity_group, `is_full_exemption`, `annual_reconfirmation`) + إصدارات **immutable بعد النشر**: value_type (PERCENTAGE/FIXED_IQD), value, `applies_to_components`, per_application_cap, stackable, priority, requires_approval/approval_role, نافذة سنوات الصلاحية, مستندات مطلوبة. النشر بـ maker-checker |
| **DiscountAssignment** | *مَن مُنح الخصم؟* — على **الطالب** (لا الـ Enrollment): مرجع التعريف، **scope**: `SINGLE_YEAR / YEAR_RANGE / ALL_YEARS`، مستندات، workflow موافقة، إلغاء بـ `revocation_effect` |
| **DiscountApplication** | *كم خُصم فعلاً على حساب سنة بعينها؟* — لكل FinancialAccount: مرجع **إصدار** التعريف المجمَّد، `frozen_base`, `computed_amount`, `applied_amount`, `truncation_reason`, `sequence`, حالة |

### 7.2 سؤال الـ All-Years (حسمه)

- تطبيق تلقائي صامت للأبد؟ **مرفوض** — الأهلية تنتهي (الأب يترك التدريس) وهو باب تلاعب.
- إعادة منح يدوية كل سنة؟ **مرفوض** — عبء كتابي ونسيان وتصحيحات رجعية.
- **المعتمد**: عند توليد حساب السنة الجديدة يُنشأ Application **تلقائياً** لكل Assignment موافَق يغطي السنة، لكن *التفعيل* يخضع لعلم التعريف `annual_reconfirmation`: false (امتياز موظفين دائم) → Applied فوراً؛ true (خصم اجتماعي) → يبقى `Pending` في قائمة عمل الموظف والرسم كامل حتى التأكيد. **الـ Assignment لا يلمس حسابات وُلدت قبله إطلاقاً** — المنح المتأخر يمر عبر مسار Adjustment (§7.5).
- إلغاء منتصف السنة: `PROSPECTIVE_ONLY` (الافتراضي — تطبيق السنة الحالية يبقى) أو `INCLUDE_CURRENT_YEAR` (عكس التطبيق بقيد adjustment وإعادة توزيع الأقساط **غير المدفوعة** فقط).

### 7.3 Stacking (أسئلتك السبعة، محسومة بمثال)

**نسبة من الأصل أم من المتبقي؟** المثال الفاصل — أصل 2,000,000، خصم A ‏10%، خصم B ثابت 100,000:
- على الأصل: 200,000 + 100,000 = خصم 300,000، صافي **1,700,000** — بأي ترتيب.
- تتابعي على المتبقي: ترتيب (A,B) يعطي 1,700,000 لكن (B,A) يعطي 10%×1,900,000=190,000 صافي **1,710,000** — نتيجتان لنفس المنح حسب الترتيب: لا يمكن الدفاع عنها أمام شباك صراف ولا تدقيق وزاري. كما أن كتب الوزارة تقول "خصم 15% من الأجور" — أي من الأصل.
- **القرار: النسب تُحسب من الأساس الأصلي القابل للخصم** (`Σ` السطور `discountable` في الـ snapshot — وليس الـ gross الكامل، كي لا يأكل إعفاءُ 100% رسومَ الهوية والوثائق).

**الخوارزمية:** (1) تحقق exclusivity: تطبيق واحد كحد أقصى لكل exclusivity_group، و`stackable=false` يعني وحيداً. (2) النسب أولاً كلٌّ من الأساس المجمَّد، ثم الثوابت؛ الترتيب داخل كل صنف بـ (priority, sequence). (3) قصّ لكل تطبيق عند `per_application_cap`. (4) سقف إجمالي الخصم (بارامتر على مستوى سنة الـ FeePolicy — يُجمَّد مع إغلاق السنة) + **الأرضية = مجموع السطور غير القابلة للخصم، ليست صفراً** (ثغرة أغلقتها المراجعة: خصم ثابت 500,000 على قسط 300,000 قابل للخصم + 200,000 هوية كان سيصفّر رسوم الهوية). التطبيق الذي يتجاوز الحد يُقصّ مع `truncation_reason`، وما بعده صفر — **لا شيء يختفي بصمت**، كل صف يسجل computed مقابل applied.
- تجاوز الخصومات قيمةَ الرسوم: مثال — أساس 1,000,000، A ‏60% (600,000)، B ‏50% (500,000) ⇒ B يُقص إلى 400,000، الصافي 0.
- **ممنوع override قيمة الخصم لطالب بعينه** — الحالة الاستثنائية تأخذ تعريفاً خاصاً بها (موافَقاً وversioned)، وإلا انهارت قصة التدقيق.

### 7.4 لماذا تغيير التعريف لاحقاً لا يمس الماضي (سؤالك 11)

‏2024-2025: `TEACHERS_CHILD` v3 = 20% ⇒ Application يشير **فيزيائياً** إلى v3 بمبلغ مجمَّد 400,000. رفعت الإدارة النسبة إلى 25%؟ يُنشر صف v4 **جديد**؛ v3 لا يُلمس. حساب السنة القديمة لا يملك أصلاً مؤشراً إلى "القيمة الحالية" — إعادة الاحتساب تقرأ v3 حصراً. المسار الوحيد لتغيير تطبيق نافذ: Reverse + Reapply موثَّقان يكتبان **صفوفاً جديدة**.

### 7.5 الإعفاء الكامل والخصم الرجعي

- **الإعفاء الكامل** = تعريف PERCENTAGE ‏100% بعلم `is_full_exemption`، غير stackable، في exclusivity group خاص، موافقته أعلى (عميد/رئيس جامعة). `applies_to_components` يسمح بإعفاء القسط دون رسوم الوثائق. **صافي 0 ⇒ لا تُولَّد خطة أقساط أصلاً** (أقساط صفرية تلوث قوائم العمل وتقارير الأعمار)؛ الحساب Settled فوراً مع بقاء gross وسطر الخصم للإحصاء وللعكس إن أُلغي الإعفاء.
- **خصم يُمنح بعد الدفع** (دفع 1,500,000 من 2,000,000 ثم خصم 50% ⇒ الصافي 1,000,000 وزيادة 500,000): **Credit لا refund تلقائي.** موافقة مالية مرتفعة مستقلة؛ الأقساط غير المدفوعة تُلغى/يُعاد توزيعها؛ الفائض `CreditEntry` بالترتيب: تعويض أقساط غير مدفوعة بنفس الحساب ← ترحيل تلقائي لحساب السنة التالية عند توليده ← refund نقدي **فقط بطلب صريح** وبموافقة المدير المالي، FIFO على الدفعات الأصلية. صرف الرصيد من مسارين متزامنين مستحيل — صف الـ Credit يُقفل `FOR UPDATE` في كلا المسارين.

---

## 8. Installment Model

- **InstallmentTemplate** (config بنفس منطق wildcard/الأوزان): `max_count`، سطور Split نسبةً أو مبلغاً أو Remainder، وقواعد استحقاق (offset من بداية السنة). **يُتحقق عند النشر** أن Σ النسب = 100 أو سطر Remainder واحد بالضبط — لا يُكتشف الخلل يوم التسجيل على مئات الطلبة.
- **Installment** (صفوف فعلية لكل حساب): رقم، `due_date`، `amount` (غير متساوٍ مسموح — مثالك 500/300/400/300 يُمثَّل مباشرة)، `paid_amount` (كاش)، حالة.
- **Invariant:** ‏Σ المبالغ = `effective_net` — يُعاد تأكيده كـ postcondition لكل تعديل خطة. كسر التقريب يذهب إلى **أقل قسط مفتوح رقماً** (لا القسط #1 الذي قد يكون مدفوعاً).
- **تعديل الخطة:** حر قبل التفعيل؛ بعد التفعيل وقبل أول دفعة → مشرف؛ بعد أول دفعة → Reschedule (مواعيد) أو Resplit **للمتبقي غير المدفوع فقط** بموافقة، ولا ينزل قسط تحت المخصَّص عليه. الصفوف القديمة تُعلَّم Superseded ولا تُحذف.
- **الحالات:** `Pending / PartiallyPaid / Paid / Waived / Superseded`. **‏Overdue مشتقة دائماً** (`remaining>0 AND due_date<today`) — لا تُخزَّن أبداً؛ الأعلام المخزّنة تتعفن وتحتاج scheduler يفشل بصمت.
- **الدفع الزائد (سؤالك 14): spillover ثم credit** — التخصيص الآلي أقدم استحقاقاً أولاً، يفيض للقسط التالي، وما بعد آخر قسط يصبح `credit_balance` قابلاً للاستهلاك أو الاسترجاع. لا يُنشأ قسط جديد بصمت أبداً.

**الفرق بين Installment و Payment** (سؤالك): القسط **التزام مجدول** (ما يجب أن يُدفع ومتى)، الدفعة **حدث نقدي** (ما دُفع فعلاً). العلاقة N:M عبر PaymentAllocation: دفعة تسدّد عدة أقساط، وقسط يُسدَّد بعدة دفعات.

---

## 9. Payment Model

**حقول:** id، `receipt_no`، السنة، الحساب المالي، (طالب+تسجيل denormalized للبحث)، `amount` (>0، دينار صحيح)، `method` (master data قابلة للإضافة: Cash/Bank/POS/Online/Transfer/Other) + `method_ref` (رقم إشعار البنك/POS)، `paid_at/posted_at`، الصراف، جلسة الصندوق، الحالة، `idempotency_key` + **`payload_hash`**، اسم الدافع (قد يغاير الطالب)، ملاحظات.

- **الترقيم:** سلسلة **لكل (سنة، شباك)**: `2025-D03-000917` — الرقابة العراقية تتوقع تسلسلاً ورقياً قابلاً للمطابقة لكل صندوق. الرقم يُمنح **عند الترحيل لا عند الإنشاء** من صف عدّاد يُقفل `FOR UPDATE` (**آخر قفل في الترتيب** — منعاً للـ deadlock)، فيرجع مع الـ rollback: التسلسل gapless عند النجاح. بصراحة: السلسلة ستتضمن أرقاماً Voided ظاهرة — وهذا صحيح تدقيقياً (void مرئي أفضل من فجوة).
- **Idempotency (حالتك 16):** المفتاح unique؛ التكرار يعيد **الوصل الأصلي نفسه** بعلامة duplicate — لا خطأ ولا وصل ثانٍ. تحصينان من المراجعة: (1) يُخزَّن hash للحمولة (حساب+مبلغ+طريقة) مع المفتاح — مفتاح مكرر بحمولة مختلفة = **خطأ صريح** لا إعادة تشغيل صامتة؛ (2) ردّ الإعادة يحمل حالة الوثيقة الحالية — إن كانت Voided يفشل الطرف العميل ولا يطبع وصلاً. شبكة أمان إضافية: نفس الحساب+المبلغ+الطريقة خلال 5 دقائق = تحذير يتطلب تأكيداً موثَّقاً.
- **دورة الحياة:** `Draft (اختياري، بلا رقم، يُطهَّر بعد 24h) → Posted (immutable) → Voided (terminal)`.
- **الترحيل transaction واحدة**: قفل صف الحساب `FOR UPDATE` ← قفل مشترك على صف السنة (يمنع سباق CloseYear/PostPayment تحت MVCC — ثغرة مكتشفة) ← الأقساط ← العدّاد أخيراً. صرافان على نفس الطالب يتسلسلان — صحيح ورخيص على هذا الحجم.
- دفعة نقدية تتطلب **جلسة صندوق مفتوحة** (§17).

---

## 10. Refund / Void Model

| | **Void** | **Refund** |
|---|---|---|
| المعنى | الدفعة ما كان يجب أن توجد (خطأ إدخال، طالب خاطئ) | الدفعة صحيحة والمال يُعاد |
| الأثر | عكس كامل فقط؛ الوصل يبقى برقمه معلَّماً VOID | جزئي أو كامل؛ وثيقة مستقلة بسلسلتها الخاصة |
| النافذة | نفس جلسة الصندوق (أو نفس اليوم بموافقة)؛ عبر الأيام → Refund حصراً | ما دامت السنة غير مغلقة مالياً؛ بعدها عبر Adjustment |
| الآلية | **VoidRequest** (الصراف يطلب، المدير المالي ينفّذ — four-eyes، وهو ما يُنفَّذ في Frappe أصلاً) | ‏workflow: `Requested → Approved → Posted` (الموافقة إلزامية، self-approval محظور) |
| شرط إضافي (سدّ ثغرة حرجة) | **يُرفض إن وُجد أي Refund مرحَّل على الدفعة** — وإلا: دفعة 1,000,000 + refund ‏400,000 + void كامل = صرف 1,400,000 مقابل مليون | Σ refunds ≤ مبلغ الدفعة (تحت القفل) |

- مبدأك (حالتك 15) مُثبَّت: Payment يبقى 500,000، Refund وثيقة 100,000، `Net Paid = 400,000` محسوبة — **لا يُعدَّل الأصل أبداً.**
- **RefundAllocation** (إضافة المراجعة): عكس الاسترجاع يستهدف **allocations الدفعة المستردَّة نفسها حصراً** (رصيدها الدائن أولاً ثم أقساطها LIFO) — العكس العام "آخر الأقساط" كان يتيح عكس تمويل دفعة أخرى ثم Void تلك الدفعة = صرف مزدوج وإفساد الرسم البياني للتخصيصات.
- الحذف الفيزيائي لأي دفعة/استرجاع: **معدوم في النظام كله**، حتى للـ Administrator (§19).

---

## 11. Iraqi-specific Cases — الحالات الثلاثون

| # | الحالة | التمثيل في النموذج |
|---|---|---|
| 1 | رسب سنة | نتيجة `Failed`؛ السنة التالية نفس المرحلة attempt=2 category=Repeat |
| 2 | أعاد مرتين | تسجيل ثالث لنفس المرحلة attempt=3، السلسلة عبر `previous_enrollment` |
| 3 | نجح وانتقل | `Passed_R1/R2`؛ تسجيل جديد stage+1 attempt=1 |
| 4 | غيّر صباحي→مسائي | بين السنوات: تسجيل السنة الجديدة بالنوع الجديد فحسب. منتصف السنة: Supersede + close-and-open + TransferCredit |
| 5 | استضافة صباحي→مسائي (وارد) | Enrollment ‏`HostedIn` بنوع الدراسة **عندنا** + HostingRecord بسياق الجهة الأم؛ الرسوم بحسب علم الاتفاقية |
| 6 | استضافة بالعكس (صادر) | تسجيله الأصلي يبقى Active + HostingRecord صادر؛ لا supersede |
| 7 | غيّر القسم | بين السنوات: تسجيل جديد وattempt يُصفَّر. منتصفها: Supersede |
| 8 | خصم سنة واحدة | Assignment بـ `SINGLE_YEAR` ⇒ Application على حساب تلك السنة فقط |
| 9 | خصم كل السنوات | Assignment ‏`ALL_YEARS` ⇒ Application يتولد مع كل حساب جديد؛ التفعيل حسب `annual_reconfirmation` |
| 10 | أكثر من خصم | Applications متعددة بخوارزمية §7.3: نسب من الأساس ثم ثوابت، exclusivity، سقف، أرضية |
| 11 | تغيّرت قيمة الخصم | إصدار تعريف جديد؛ القديم مجمَّد فيزيائياً بمرجع v القديمة (§7.4) |
| 12 | تغيّرت الرسوم | FeePolicyVersion جديد للسنة الجديدة؛ snapshots القديمة لا تتأثر |
| 13 | دفع قسطاً جزئياً | Allocation جزئي؛ القسط `PartiallyPaid` والمتبقي بنفس الاستحقاق |
| 14 | دفع أكثر من القسط | Spillover لما بعده استحقاقاً؛ الفائض النهائي `credit_balance` |
| 15 | دفع ثم Refund | وثيقة Refund موافَقة بعكس مقصور على allocations الدفعة؛ الأصل لا يُمس |
| 16 | دفعة مكررة (network retry) | idempotency_key + payload_hash: الوصل الأصلي يُعاد؛ حمولة مختلفة = خطأ؛ heuristic ‏5 دقائق |
| 17 | تعديل Payment قديم | مرفوض في طبقتي API والإقناع (Frappe amend محجوب)؛ البدائل Void/Refund/Adjustment |
| 18 | سنة أُغلقت | `FinanciallyClosed`: لا إنشاء/تعديل/void فيها؛ **تحصيل ديونها بوصل السنة الحالية مشروع**؛ التصحيح عبر AdjustmentOpen موثَّق |
| 19 | موجود لم يسجل | حالة شرعية متوقعة: Student بلا Enrollment للسنة — لا يُخترع صف |
| 20 | Enrollments تاريخية متعددة | صف لكل (سنة × supersede) مسلسل بـ previous/supersedes — لا overwrite أبداً |
| 21 | منقول | `TransferredOut` + مرجع الأمر؛ الحساب يُسوّى؛ أو `TransferIn` قادماً |
| 22 | مؤجل | `Deferred` + `NotApplicable`؛ لا يُحتسب محاولة؛ ماليته بارامتر صريح |
| 23 | منقطع | `DroppedOut`؛ دينه باقٍ ظاهراً |
| 24 | خريج | `Completed` بالمرحلة الأخيرة؛ حالة الطالب مشتقة؛ براءة ذمة حسب السياسة |
| 25 | عاد بعد انقطاع | تسجيل جديد مربوط بآخر محاولة محتسبة + فحص حجب الدين |
| 26 | دين سنة سابقة | يبقى على حساب سنته؛ الذمة الكلية تجميع قراءة؛ الحجب سياسة سنوية |
| 27 | رسوم المعيد | صف FeePolicy بـ `student_category=REPEAT` — يفوز بالـ specificity تلقائياً |
| 28 | رسوم حسب نوع الدراسة | بُعد `study_type` في FeePolicy — بيانات لا كود |
| 29 | رسوم حسب القسم/المرحلة | بُعدا `department/stage` — نفس الآلية |
| 30 | إعفاء كامل | تعريف 100% بـ `is_full_exemption`؛ صافي 0 بلا خطة أقساط؛ gross محفوظ للإحصاء |

---

## 12. State Machines

- **Student**: `Active → Separated ⇄ Active` (عودة بأمر) | `Active → TransferredOut` | `Active → Graduated` (مشتقة) | `أي → Deceased` (وثيقة، terminal).
- **AcademicYear**: `Draft → Open → FinanciallyClosed → Closed` + استثناء `Closed → AdjustmentOpen → Closed` (مؤقت، أوامر whitelisted). لا رجوع.
- **Enrollment**: `Draft → Active`؛ `Active → {Superseded, Deferred, DroppedOut, Withdrawn, TransferredOut, Completed}`؛ `Deferred → Active` (نفس السنة) أو terminal عند إغلاق السنة. Draft وحده قابل للحذف.
- **FinancialAccount**: `Pending → Active ⇄ Settled`؛ `Pending|Active → Cancelled`. تجميد السنة guard متعامد.
- **Installment**: `Pending ⇄ PartiallyPaid ⇄ Paid` (العكوس تحرّكها reversals) | `→ Waived` | `→ Superseded` (resplit). ‏Overdue مشتقة.
- **Payment**: `Draft → Posted → Voided`؛ Draft قابل للحذف/الانتهاء. Posted لا يُعدَّل.
- **Refund**: `Requested → Approved → Posted`؛ `Requested → Rejected`؛ `Approved → Cancelled` قبل الترحيل. Posted نهائي (تصحيحه Adjustment).
- **DiscountAssignment**: `Draft → Submitted → Approved | Rejected`؛ `Approved → Revoked | Expired`.
- **DiscountApplication**: `Pending → Applied | Declined`؛ `Applied → Reversed` (إلغاء/supersede/تسوية). إعادة المنح = صف جديد.
- **CashierSession**: `Open → Closed → Approved`؛ إغلاق بفرق ≠ 0 يستلزم سبباً وموافقة.
- **ImportBatch**: `Uploaded → Validating → NeedsReview → Confirmed → Importing → Imported | Failed` (+Cancelled قبل التنفيذ؛ أي تعديل صف يعيد Validating).

---

## 13. Business Rules (الجوهرية، مرقّمة)

1. الوحدة المالية = Enrollment؛ حساب مالي واحد لكل تسجيل (1:1).
2. تسجيل حيّ واحد كحد أقصى لكل (طالب، سنة) — قيد فرادة فعلي لا اتفاقية.
3. لا يُعدَّل ولا يُحذف أي مستند مالي مرحَّل؛ التصحيح بوثيقة معاكسة موثَّقة.
4. الرسوم والخصومات configuration منشورة بإصدارات immutable؛ الحساب يجمّد ما استُخدم.
5. `effective_net = net_snapshot + Σ adjustments`؛ ‏Σ الأقساط = effective_net دائماً.
6. النسب من الأساس القابل للخصم الأصلي؛ الثوابت بعدها؛ الأرضية = السطور غير القابلة للخصم.
7. إجمالي الخصم لا يتجاوز الأساس القابل للخصم؛ القصّ مسجَّل بسببه، لا حذف صامت.
8. لا Application جديد على حساب بعد توليده إلا عبر مسار Adjustment الموافَق.
9. الدفعة لحساب واحد؛ ديون متعددة السنوات = وصولات متعددة (v1).
10. رقم الوصل يُمنح عند الترحيل من عدّاد مقفول لكل (سنة، شباك)؛ الملغى يحتفظ برقمه.
11. Void يتطلب صفر refunds مرحَّلة على الدفعة؛ عكس الـ Refund مقصور على allocations دفعته.
12. كل أمر مالي = DB transaction واحدة؛ قفل الحساب أولاً، صف السنة مشتركاً، العدّاد أخيراً.
13. المال أعداد صحيحة IQD (bigint)؛ النسب عبر أداة Decimal واحدة ROUND_HALF_UP؛ float محظور حتى في JSON.
14. `FinanciallyClosed` يمنع الترحيل المؤرَّخ للسنة وتعديل بياناتها؛ لا يمنع تحصيل ديونها بوصل السنة الحالية.
15. الدين يبقى على حساب سنته؛ الحجب سياسة سنوية تُفحص عند التسجيل.
16. الكاش يُحدَّث متزامناً تحت القفل؛ المصالحة الليلية كاشفة؛ الأرقام المطبوعة من الصفوف الخام حصراً.
17. فصل الواجبات: مانح الخصم ≠ موافقه؛ الصراف لا يلغي ولا يسترجع؛ الـ Admin لا يسجّل دفعات.
18. صرف CreditEntry يمر بقفل صفّه — استحالة الصرف المزدوج.
19. الدفعة النقدية تستلزم جلسة صندوق مفتوحة.
20. الاستيراد والتوليد الجماعي: validate → preview (hash لكل صف) → confirm؛ الالتزام يقارن hash الصف ويتخطى المتغيِّر بسببٍ ظاهر؛ لا نجاح جزئي صامت أبداً.

---

## 14. Database Schema Proposal (ERD منطقي — بلا SQL كامل)

≈ 30 جدولاً؛ الأعمدة القياسية (created/updated at/by) محذوفة اختصاراً:

| جدول | PK | FK / Unique / ملاحظات |
|---|---|---|
| `student` | id | uq(student_no)؛ حقول بحث denormalized + normalized |
| `student_identity_version` | id | FK student؛ append-only؛ قرار المحكمة؛ uq(student, version_no) |
| `academic_year` | id | uq(code)؛ status يتضمن **FinanciallyClosed**؛ أختام مرحلتي الإغلاق |
| `college` / `department` / `study_type` / `student_category` / `payment_method` | id | masters؛ uq(code)؛ department يحمل `years_count` |
| `enrollment` | id | FKs: student, year, college, department, study_type, category, superseded_by (self, uq), **previous_enrollment** (self)؛ uq(student, year, sequence_no)؛ uq(student, year, is_current)-flag؛ `enrollment_kind` |
| `hosting_record` | id | uq(enrollment)؛ direction + سياق أم/مضيف + علم سياسة القبض |
| `fee_policy_version` | id | أبعاد بـ sentinel `ALL`؛ uq(البُعد السداسي، version)؛ status |
| `fee_component` | id | FK policy_version؛ uq(version, code)؛ discountable/refundable |
| `financial_account` | id | FK enrollment + **enrollment_active flag-trick uq** (يسمح regenerate بعد إلغاء)؛ denorm: year/dept/study_type (snapshot-stable، يوفر join في كل تقرير)؛ totals مجمَّدة + كاش + reconciliation stamp |
| `fee_snapshot_line` | id | FK account؛ uq(account, component_code)؛ immutable |
| `discount_definition` | id | uq(code)؛ header فقط |
| `discount_definition_version` | id | FK definition؛ uq(def, version_no)؛ insert-only |
| `discount_assignment` | id | FK student, definition؛ scope + سنوات؛ منع التداخل app-layer |
| `discount_application` | id | FK account, assignment, definition_version؛ **state** + uq(account, assignment, is_active-flag) — يسمح Reverse+Reapply |
| `installment_template` (+lines) | id | uq(code)؛ التحقق من Σ عند النشر |
| `installment` | id | FK account؛ uq(account, installment_no)؛ بلا عمود Overdue |
| `payment` | id | FK account, method, series, session؛ uq(series, receipt_no)؛ uq(idempotency_key) + payload_hash |
| `payment_allocation` | id | FK payment, installment؛ **بلا** uq(payment, installment) — append-only مع الاسترجاعات؛ amount>0 |
| `refund` | id | FK payment, series؛ حالات workflow الموافقة؛ uq(idempotency_key) |
| `refund_allocation` | id | FK refund, installment؛ Σ ≤ allocation الأصلي لكل قسط |
| `account_adjustment` | id | FK account (+pair_link للزوج TransferCredit)؛ type؛ مبلغ موقَّع؛ موافقة؛ **يدخل في views الرصيد** |
| `credit_entry` | id | FK account/student؛ status Open/Consumed/Refunded؛ يُقفل عند الصرف |
| `void_request` | id | FK payment؛ طالب/منفّذ/سبب (four-eyes) |
| `cashier_session` | id | uq(cashier, is_open)-flag؛ أرصدة متوقعة/فعلية/فرق |
| `number_series` | id | uq(series_code, year, desk)؛ عدّاد يُقفل FOR UPDATE |
| `audit_log` | id | polymorphic (doctype, docname)؛ before/after JSON؛ سبب؛ **hash-chain** (كل صف يحمل hash سابقه) |
| `import_batch` / `import_row` | id | uq(batch, row_no)؛ uq(batch, dedup_hash)؛ per-row status + preview_hash |

---

## 15. Constraints & Indexes

**CHECKs في SQL:** المبالغ > 0 (دفع/استرجاع/تخصيص/قسط) و≥ 0 (مكونات/سطور)؛ `attempt_number ≥ 1`؛ `stage` ضمن مدى القسم؛ `paid_amount BETWEEN 0 AND amount`؛ الحالات IN(...)؛ سنوات المدى مرتبة.

**قواعد لا تقبل التمثيل في MariaDB CHECK (عابرة للصفوف) — طبقة التطبيق داخل الـ transaction إلزاماً:** Σ الأقساط = effective_net؛ Σ تخصيصات الدفعة ≤ مبلغها وΣ تخصيصات القسط ≤ مبلغه؛ Σ الاسترجاعات ≤ الدفعة؛ بوابة السنة المغلقة؛ جمود الدفعة المرحَّلة؛ رياضيات الـ stacking؛ منع تداخل الـ assignments؛ فوز policy واحدة؛ ذرّية الـ supersede؛ **backstop على مستوى MariaDB triggers** ترفض UPDATE/DELETE على جداول الدفع/الاسترجاع/التدقيق (لأن Frappe يتصل بمستخدم قاعدة واحد يملك كل شيء — DB grants وحدها لا تحمي، §19).

**الفهارس:**
- بحث: uq(student_no)؛ `full_name_norm(64)`؛ `(full_name_norm, mother_name_norm)` — استعلام التمييز العراقي القياسي؛ `phone_norm`؛ **`phone_reversed`** (البحث بآخر 4 أرقام يصبح prefix scan على المقلوب)؛ `payment(external_ref)` لإشعارات المصرف.
- تطبيع عربي (عمود مخزَّن يُملأ عند الكتابة، ونفس الدالة تطبَّع نص البحث): أ/إ/آ/ٱ→ا، ؤ→و، ئ/ى→ي، ة→ه، حذف التشكيل والتطويل، توحيد الفراغات. **لا تعتمد على الـ collation** — سلوكها مع ة/ه وى/ي غير مضمون بين الإصدارات. البحث prefix أولاً ثم `%..%` عند الصفر؛ وعند البطء على 200 ألف اسم: FULLTEXT ngram قبل أي محرك خارجي.
- تقارير: `enrollment(year, college, department, stage)` + `(year, study_type)`؛ `installment(due_date, status)` لمسح المتأخرات؛ `payment(posted_at, cashier)`، `payment(account, status)`؛ `audit_log(at)` مرشَّح للتقسيم السنوي.

---

## 16. Reporting Model

**أربعة views قانونية** يقرأ منها كل شيء: `v_account_balance` (يجمع snapshot + خصومات + تخصيصات − استرجاعات **+ adjustments** — إدراج الـ adjustments شرط صحة، وإلا ظهر دينان وهميان لكل supersede)، `v_installment_status` (بحالة مشتقة تشمل Overdue)، `v_enrollment_effective` (يستثني Superseded — **للعدّ فقط**)، `v_debt`.

**قاعدة العدّ مقابل المال (حسمها مدقق التاريخ):** الأعداد من `v_enrollment_effective`؛ **المال من كل حسابات السنة** مع أزواج TransferCredit تُصفّي ثنائية الـ supersede إلى مركز اقتصادي واحد. الخياران الساذجان يشوّهان: الضم على كل التسجيلات يضاعف الـ gross، والضم على الفعّالة فقط يُسقط نقداً حقيقياً قُبض على الحساب القديم فيستحيل مطابقة تقرير السنة مع أدراج الصرافين. والمستضافون الواردون يُعرضون سطوراً منفصلة (رسومهم قد لا تكون لنا).

**التقارير المطلوبة (R1–R10):** كشف الطالب الشامل (هوية + سلسلة أكاديمية بخط الـ supersede + لكل حساب: مكونات/خصومات بإصداراتها/أقساط/دفعات حتى الملغاة/استرجاعات/رصيد + Timeline موحَّد من الأحداث والتدقيق) — من الصفوف الخام حصراً؛ ملخص الأقسام كلها؛ قسم واحد (مرحلة×نوع دراسة + drill-down)؛ نوع الدراسة؛ المرحلة (+ عدّاد المعيدين وإيرادهم منفصلاً)؛ السنة (وديون السنوات السابقة المحصَّلة خلالها **سطراً منفصلاً لا يُخلط بصافي السنة**)؛ الأقساط (Expected/Paid/Remaining/Overdue بحسب شهر الاستحقاق)؛ الديون (مع علم الحجب معروضاً)؛ الخصومات (من صفوف Application حصراً — لا يُعاد الحساب من التعريفات أبداً)؛ الصندوق اليومي (قاعدة صلبة: void بنفس اليوم يُصفّى من درج الصراف؛ عبر الأيام Refund حصراً).

**تقارير أضفتها (نسيتَها):** أعمار الذمم (0–30/31–90/91–180/180+/سنوات سابقة)؛ يومية القبض الزمنية؛ **سجل الاسترجاعات وسجل الإلغاءات** (فجوة الوقت بين الترحيل والإلغاء = كاشف التلاعب الأول)؛ سجل الإعفاءات (مستحق وزاري)؛ **مصالحة الكاش** (فارغ = سليم)؛ التدفق النقدي المتوقع من استحقاقات الأقساط؛ نسبة التحصيل زمنياً وقسمياً؛ أعداد الوزارة؛ اتجاه كلفة الخصومات عبر السنوات.

**المعمارية (عملية، لا over-engineering):** حتى ~50 ألف طالب: استعلامات مباشرة مفهرسة + الـ views — MariaDB يجيب بأقل من ثانيتين على ملايين قليلة من الصفوف سنوياً. كاش totals الحساب من اليوم الأول (transactional — تحتاجه شاشات القوائم وفحص الحجب). مصالحة ليلية إلزامية. جداول تجميع materialized **فقط** عند تجاوز تقرير 5 ثوانٍ فعلياً — وأولها `agg_daily_collection` وحده. لا star schema ولا OLAP — حجم خاطئ.

---

## 17. Permissions

| Role | يستطيع | لا يستطيع (صراحةً) |
|---|---|---|
| **Cashier** | تسجيل دفعة + طباعة وصل؛ قراءة ماليات الطالب؛ طلب Void لدفعته بنفس اليوم (VoidRequest)؛ كشف وردّيته | تغيير رسوم؛ الموافقة على خصم؛ Void مباشر أو refund؛ رؤية سجل التدقيق |
| **Finance Manager** | ‏FeePolicy والقوالب؛ الموافقة على الخصومات (**ليس ما منحه هو**)؛ تنفيذ Void؛ الموافقة على Refund؛ توليد الحسابات والدفعات الجماعية؛ توقيع مشارك للإغلاق | تسجيل نتائج أكاديمية |
| **Registrar** | الطلبة والتسجيلات والـ supersede والتأجيل/الانقطاع/العودة/التخرج؛ الاستيراد | أي عملية مالية |
| **Academic Officer** | النتائج والترقية الجماعية والإعادة | المال (قراءة فقط) |
| **Admin** | الإعدادات والسنة والأدوار | **تسجيل الدفعات — محجوب عمداً** (فصل واجبات) |
| **Report Viewer** | تقارير بنطاق دوره | كل ما عداها |
| **Auditor** | قراءة كل شيء + سجل التدقيق + إشعار آلي عند AdjustmentOpen | أي كتابة |

**عمليات four-eyes إلزامية:** موافقة الخصم؛ نشر إصدار تعريف (maker-checker)؛ تنفيذ Void؛ ترحيل Refund؛ إغلاق السنة (توقيعان)؛ أي Adjustment بعد الإغلاق؛ الخصم الرجعي على حساب مدفوع (المدير المالي)، والـ refund النقدي للرصيد الدائن (المدير المالي).

---

## 18. API / Domain Commands

**CRUD مقابل Commands:** ‏`PUT /payment/{id}` دعوة مفتوحة لتعديل التاريخ. `POST /commands/void-payment` نية لها شروط وصلاحية وأثر تدقيقي. القاعدة: **كل كتابة على أي كيان مالي أو تسجيل تمر بأمر مسمّى؛ القراءات queries حرة قابلة للكاش.** الـ masters تقبل تحريراً عادياً لكن بلا حذف بعد أول استخدام (soft-retire).

**الكتالوج** (كلٌّ بـ actors/preconditions/effects/idempotency، وكلها تُصدر Audit):
- *أكاديمية:* RegisterStudent (فحص شبيه الاسم+الأم+المواليد مع override موثَّق)، EnrollStudent (سنة Open، لا تسجيل حي، فحص حجب الدين)، SupersedeEnrollment (قسم/نوع/تصحيح LateR2Result)، RecordAcademicResult (لكل دور)، PromoteStudentsBulk (لكل صف: تخطٍّ + تقرير)، RepeatStudent، DeferStudent (المعالجة المالية بارامتر)، WithdrawStudent، MarkDropout، ReactivateReturningStudent، GraduateStudent (شرط براءة الذمة سياسة).
- *مالية:* DefineFeePolicy (إصدار جديد دوماً)، GenerateFinancialAccount (+Bulk)، AdjustInstallmentPlan (غير المدفوع فقط، Σ محفوظ)، AssignDiscount / ApproveDiscountAssignment / RevokeDiscountAssignment، RecordPayment، RequestVoid / ExecuteVoid، RequestRefund / ApproveRefund / PostRefund، PostAccountAdjustment، SettleSupersededAccount.
- *السنة:* OpenAcademicYear، FinanciallyCloseYear (شرط: صفر Drafts + مصالحة مقروءة ومُقرَّة)، CloseAcademicYear، ReopenYearForAdjustment (مؤقت، whitelisted، إشعار Auditor).
- *جماعية:* ImportStudentsBatch، GenerateAccountsBulk.

**‏Workflow الاستيراد (طلبك 16):** Upload → Validate → Preview مع أخطاء لكل صف → قرارات (create/update/skip/error) → Confirm (مشروط بصفر أخطاء معلّقة) → Import بخيوط **commit لكل صف** (صف مسموم لا يُسقط 199 سليماً) + heartbeat وreaper وResumeBatch للدفعات المعلّقة — لا دفعة تعلق "Importing" للأبد. مصفوفة التكرارات: رقم مكرر داخل الملف = خطأ؛ رقم موجود بنفس الاسم/الأم = update تسجيلاً فقط؛ نفس الرقم باسم مختلف = خطأ يتطلب تأكيداً موثَّقاً أو رقماً جديداً؛ enrollment قائم متطابق = تخطٍّ، ومتعارض = خطأ (الـ supersede بالأمر النظامي، **ليس بالاستيراد**). **الاستيراد لا يولّد حسابات مالية أبداً** — ذلك أمر منفصل صريح.

**الإعداد المالي الجماعي (طلبك 17):** اختيار النطاق (سنة/قسم/مرحلة/نوع) — السياسة **تُعرض ولا تُختار** (المصدر هو الـ resolution)؛ **Dry-run إلزامي**: `will_create 412 / already_has 37 / blocked 9` بأسبابها، ولكل صف hash للمعاينة؛ ثم enqueue؛ والالتزام **يقارن hash كل صف** ويتخطى المتغيّر بسبب ظاهر (بدون هذا تصبح موافقة المدير مسرحية إن تغيّرت السياسة بين المعاينة والتنفيذ)؛ تقرير ختامي created/skipped/failed — الحالة `CompletedWithSkips` وليست "Done" عمياء.

---

## 19. Frappe/ERPNext Mapping

**القرار: تطبيق مخصص `uni_fees` على Frappe v15+.** ‏ERPNext Education انفصل عن النواة وصيانته بطيئة ونموذجه (برامج/فصول غربية) لا يعرف المرحلة والمعيد ونوع الدراسة والـ supersede. ‏Payment Entry يجرّ شجرة حسابات وعملات لا نحتاجها. إن شغّلت الجامعة ERPNext محاسبياً لاحقاً: adapter أحادي الاتجاه يُصدر Journal Entry من دفعاتنا المرحَّلة — **ولا عكس أبداً**.

| كيان | نوع DocType | ملاحظات |
|---|---|---|
| Student, AcademicYear, StudyType, College, Department, FeePolicy | Masters | حالة السنة حقل + أوامر server، ليست docstatus |
| Enrollment | Master بحالة | دورة حياته أكبر من docstatus الثلاثي؛ التحولات server-side حصراً |
| FinancialAccount | **Submittable** | ‏FeeSnapshotLine = child (يُكتب مرة، يُقرأ مع أبيه) |
| **Installment** | **Standalone** (حاسم) | ثلاثة قواتل للـ child: ‏Link لا يستهدف صف child؛ تحديث paid/status على أبٍ submitted يصطدم بـ update-after-submit؛ قوائم الاستحقاق تحتاج list queries أولى |
| **DiscountApplication** | **Standalone** مرتبط بالحساب (عدّلته المراجعة — كان child) | الموافقة منتصف السنة على child مجمَّد لأب docstatus=1 مستحيلة بلا خرق القواعد؛ ‏`effective_net` يُحسب تحت القفل من الوثائق |
| Payment | Submittable 0/1/2 | ‏PaymentAllocation = child يُكتب ذرّياً مع الترحيل؛ **cancel = Void** (عكس منطقي في on_cancel)؛ **amend محجوب** على Payment/Refund (سلاسل amended_from وإعادة التسمية ‑CANC تفسد التسلسل المالي — الخطأ يُلغى ويُعاد وصلاً جديداً) |
| Refund | Submittable + workflow موافقة | ‏RefundAllocation = child له (عكوسه صفوفه هو — لا تُحشر في Payment المرحَّل) |
| VoidRequest, AccountAdjustment, CreditEntry | Submittable | four-eyes طبيعي |
| DiscountAssignment | Submittable + **Workflow doctype** | مانع self-approval بالسيرفر |
| AuditLogEntry, ImportBatchRow | Standalone insert-only | الصفوف standalone لا child (حدود واجهة child بعد مئات الصفوف) |

**السلامة المالية داخل Frappe:**
- كل قواعد المال Python controllers (`validate/before_submit/on_submit/on_cancel`)؛ ‏Client Scripts زينة UX — افترض عميلاً معطوباً أو عدائياً.
- أقفال: `frappe.qb ... for_update` على صف الحساب أولاً ثم الأقساط ثم العدّاد؛ ممنوع `frappe.db.commit()` وسط controller؛ `db.set_value` محظور مالياً إلا لكتابة الكاش داخل مقطع مقفول.
- فهارس unique عبر **patches** (لا علم الحقل وحده): idempotency_key، (series, receipt_no)، enrollment_active.
- **حقيقة مزعجة يجب قبولها:** ‏Frappe يتصل بمستخدم MariaDB واحد يملك كل شيء، وHooks الـ ORM يتجاوزها raw SQL — لذا الـ backstop الحقيقي: **triggers** ترفض UPDATE/DELETE على جداول الدفع والتدقيق، وسلسلة hash في audit_log (كل صف يحمل بصمة سابقه) + شحن السجل خارج الصندوق. صلاحيات Frappe خط أول، ليست الضمانة. واحجب حذف docstatus=2 عن System Manager على الوثائق المالية.
- **العملة:** حقول Currency في Frappe تطفو float في Python. القرار: **كل المال Long Int (bigint) بالدينار الصحيح** — ‏Int العادي int(11) يفيض عند 2.147 مليار IQD وأي مجموع كلية/سنة يتجاوزه حتماً. النسب عبر أداة Decimal واحدة ROUND_HALF_UP — لا `round()` البايثونية (banker's) ولا `flt` (تهيئتها site-specific).
- جماعيات: `frappe.enqueue` بطابور طويل، commit لكل صف، progress منشور، وreaper مجدول يلتقط الدفعات بلا نبض.
- تقارير: Query Reports للتجميعات المباشرة، Script Reports للأعمار والمصالحة وكشف الطالب (+Print Format عربي)؛ فلتر السنة إلزامي يحدّ كل مسح.
- عربية: RTL جيد؛ الأسماء التقنية إنكليزية والعناوين مترجمة؛ `title_field` = الاسم + الأم لبحث الـ Link؛ أرقام غربية على الوصولات المطبوعة (لبس ٠/0).
- **الجامعة متعددة الكليات = site واحد** والكلية حقل — تعدد الـ sites يقتل الاستعلامات العابرة والاستضافة الداخلية؛ التقييد بـ User Permissions على College عند الحاجة.

---

## 20. Risks & Design Mistakes

**أخطاء في أفكارك صحّحناها (بصراحة كما طلبت):**
1. «سنة Open/Closed بحالتين» — يكسر واقع الدور الثاني؛ الحل مرحلتان + سنتان متزامنتان + AdjustmentOpen.
2. «الاستضافة نوع دراسة» (صياغة سؤالك توحي بذلك) — تفسد زوج المصدر/الهدف وFeePolicy؛ الحل overlay.
3. «خصم All-Years يطبق تلقائياً» — ثغرة أهلية وتلاعب؛ الحل materialize عند التوليد + بوابة إعادة التأكيد السنوية.
4. «Max Number of Installments» وحده لا يكفي — القالب يحتاج تحقق نشرٍ وقاعدة كسر تقريب وقواعد تعديل بعد الدفع.
5. «Fee = مبلغ واحد» — عملياً مكونات متعددة بعضها غير قابل للخصم وغير قابل للاسترجاع؛ بدونها يُفسد الإعفاءُ الكامل رسومَ الوثائق.

**فخاخ اكتشفتها المراجعة العدائية وأُغلقت في هذه الوثيقة** (لولاها لانفجرت بعد سنة تشغيل): ‏Void بعد Refund = صرف مزدوج (قاعدة 11)؛ عكس refund عام يفسد تخصيصات دفعات أخرى (RefundAllocation المقصور)؛ سباق CloseYear/PostPayment تحت MVCC (قفل صف السنة)؛ غياب جدول Adjustment الذي يعتمد عليه supersede (دين وهمي مزدوج + حجب خاطئ)؛ منع تحصيل ديون السنة المغلقة (تفريق التأريخ عن التحصيل)؛ ‏unique(payment, installment) يمنع نموذج العكوس؛ ‏NULL wildcards تبطل unique الـ policy (sentinel)؛ خوارزميتا specificity متعارضتان (توحيد الأوزان)؛ فيض int(11) على المجاميع (bigint)؛ دفعات bulk عالقة للأبد (heartbeat/resume)؛ commit يعيد resolve بصمت بعد المعاينة (hash لكل صف).

**مخاطر باقية بعين مفتوحة:** (1) سؤال سياسة قبض الاستضافة غير محسوم — يجب حسمه مع الجامعة قبل أول اتفاقية، والنموذج جاهز لكل الإجابات. (2) صلاحية Administrator في Frappe فوق كل حماية تطبيقية — الـ triggers وسلسلة الـ hash وشحن السجلات هي الجواب، مع تقييد من يحمل الحساب. (3) هجرة البيانات القديمة (سنوات ورقية/Excel بلا سلاسل نظيفة) ستحتاج أعلام `migrated` وoverride موثَّقاً لتحقق attempt_number. (4) انضباط الصرافين على idempotency_key يعتمد على واجهة الشباك — يجب أن تولّد المفتاح آلياً لا يدوياً.

---

## 21. Recommended MVP

**المرحلة 1 (النواة — بدونها لا نظام):** الهويات والـ masters؛ Enrollment بكامل حالاته + supersede؛ السنة بمرحلتي الإغلاق؛ FeePolicy/components/resolution + snapshot؛ FinancialAccount + adjustments + credits؛ الخصومات بطبقاتها الثلاث والتراص؛ الأقساط والتخصيص؛ Payment بالترقيم والقفل والـ idempotency؛ Refund بالموافقة وVoidRequest؛ **Cashier Session (النسخة الخفيفة — قرار: ضمن الـ MVP**، فهي ضابط الاحتيال النقدي الأول على شباك حقيقي وكلفتها doctype-ان)؛ الاستيراد المرحلي؛ التوليد الجماعي بالمعاينة؛ التقارير R1–R10 + سجلا الإلغاء والاسترجاع + المصالحة؛ الأدوار والـ four-eyes؛ audit بسلسلة hash.

**مؤجَّل عمداً:** دفعة cross-account؛ تكامل ERPNext GL؛ بوابة طالب/دفع إلكتروني؛ عدّ فئات النقد وتسليم الورديات؛ جداول التجميع الـ materialized (حتى يثبت البطء)؛ إشعارات SMS للمتأخرات.

## 22. Future Extensions

دفعة موزعة على حسابات الطالب بقواعد صارمة (ترتيب أقفال بالـ id، الرصيد للحساب الأحدث، وصل واحد بسطور)؛ بوابة دفع إلكتروني (idempotency الحالي يستوعبها مباشرة)؛ adapter محاسبة حكومية/ERPNext؛ منح وكفالات جهات خارجية (sponsor يحل محل الطالب دافعاً)؛ أرشفة السنوات الباردة؛ تحليلات تنبؤية للتحصيل؛ تطبيق طالب للاطلاع على كشفه.

## 23. Final Architecture Diagram

```
                    ┌──────────── CONFIGURATION (versioned, immutable after publish) ───────────┐
                    │ FeePolicyVersion→FeeComponent   DiscountDefinition→Version   InstTemplate │
                    └──────────────────────────┬────────────────────────────────────────────────┘
                                               │ resolve + SNAPSHOT (frozen at generation)
 IDENTITY                 CONTEXT              ▼                    TRANSACTIONS (append-only)
┌─────────┐   1:N  ┌──────────────────┐ 1:1 ┌────────────────────┐
│ Student │───────▶│    Enrollment    │────▶│  FinancialAccount  │──1:N──▶ FeeSnapshotLine
│ +IdVers │        │ (year context)   │     │ net frozen +       │──1:N──▶ DiscountApplication (→DefVersion)
└─────────┘        │ supersede chain  │     │ Σadj = effective   │──1:N──▶ AccountAdjustment ⇄ pair
     ▲             │ prev_enrollment  │     └─────────┬──────────┘──1:N──▶ CreditEntry (lockable)
     │             │ HostingRecord    │               │ 1:N
DiscountAssignment │ (overlay)        │         ┌─────▼─────┐   N:M(PayAlloc)  ┌─────────┐
 (student grant,   └────────┬─────────┘         │Installment│◀════════════════▶│ Payment │→ receipt series
  scope, approval)          │ N:1               └───────────┘   N:M(RefAlloc)  │ (year,desk)
                     AcademicYear                      ▲◀═══════════════════════╪═ Refund │
              Draft→Open→FinClosed→Closed              │            CashierSession └──┬────┘
                        │                              │                              │
                        └──────── AuditLog (hash-chained, append-only) ◀──────────────┘
        Projections: cached totals (sync, reconciled nightly) · v_account_balance · v_debt · reports
```

---

# Recommended Final Domain Model (الخلاصة المكثفة)

- **Student** = هوية فقط (رقم جامعي، هوية موثّقة versioned بقرارات المحاكم، اتصالات). لا شيء أكاديمي أو مالي عليه.
- **Enrollment** = (طالب × سنة × سياق): كلية/قسم/مرحلة/نوع دراسة/فئة/محاولة + ثلاث حالات منفصلة + سلسلة supersede وسلسلة previous_enrollment. تسجيل حيّ واحد لكل (طالب، سنة). الاستضافة overlay عليه.
- **AcademicYear**: ‏Draft → Open → FinanciallyClosed → Closed؛ سنتان قد تُفتحان معاً؛ تصحيح ما بعد الإغلاق عبر AdjustmentOpen الموقوت.
- **FinancialAccount** (1:1 مع Enrollment) = snapshot مجمَّد للرسوم والخصومات + `effective_net = net + Σ adjustments` + كاش متزامن مُصالَح ليلياً + CreditEntries مقفولة الصرف.
- **الرسوم والخصومات** = configuration بإصدارات immutable؛ ‏resolution بأوزان قوى الاثنين؛ النسب من الأساس القابل للخصم؛ الأرضية = غير القابل للخصم؛ الإعفاء = 100% بلا خطة أقساط؛ الخصم طبقات ثلاث: Definition/Assignment/Application.
- **الأقساط** = صفوف فعلية غير متساوية Σها = effective_net؛ ‏Overdue مشتقة؛ الدفع الزائد spillover ثم credit.
- **Payment/Refund/Void** = وثائق immutable؛ ترقيم عند الترحيل لكل (سنة، شباك)؛ idempotency بمفتاح+hash؛ ‏Refund بموافقة وعكس مقصور على تخصيصات دفعته؛ ‏Void بلا refunds سابقة عبر VoidRequest؛ الحذف معدوم.
- **الأوامر لا CRUD**، بأقفال حساب→سنة→عدّاد، وfour-eyes على كل حساس، وAudit بسلسلة hash، وbackstop بـ DB triggers.
- **التنفيذ**: تطبيق Frappe مخصص؛ ‏Installment وDiscountApplication وAuditLog وImportRow كيانات standalone؛ المال bigint دينار صحيح؛ ‏amend محجوب على الوثائق المالية؛ site واحد والكلية حقل.

هذا النموذج يحفظ كل تاريخ، يجعل تلاعب الماضي مستحيلاً بنيوياً لا اتفاقياً، ويحاسب كل دينار من لحظة الـ snapshot حتى وصل القبض — وهو قابل للتشغيل في جامعة عراقية لسنوات دون أن يعيد كتابة نفسه.
