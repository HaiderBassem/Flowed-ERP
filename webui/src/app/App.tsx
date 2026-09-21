import { useEffect, useState } from "react";
import { Navigate, Route, Routes } from "react-router-dom";

import { SignInScreen } from "@/domains/auth/SignInScreen";
import { ChangePasswordScreen } from "@/domains/auth/ChangePasswordScreen";
import { DeskScreen } from "@/domains/desk/DeskScreen";
import { StudentSearchScreen } from "@/domains/students/StudentSearchScreen";
import { StudentScreen } from "@/domains/students/StudentScreen";
import { RegisterStudentScreen } from "@/domains/students/RegisterStudentScreen";
import { EditStudentScreen } from "@/domains/students/EditStudentScreen";
import { AccountScreen } from "@/domains/accounts/AccountScreen";
import { GenerateAccountScreen } from "@/domains/accounts/GenerateAccountScreen";
import { PaymentScreen } from "@/domains/payments/PaymentScreen";
import { VoidInboxScreen } from "@/domains/voids/VoidInboxScreen";
import { RefundInboxScreen } from "@/domains/refunds/RefundInboxScreen";
import { EnrollScreen } from "@/domains/enrollments/EnrollScreen";
import { EnrollmentScreen } from "@/domains/enrollments/EnrollmentScreen";
import { GrantDiscountScreen } from "@/domains/discounts/GrantDiscountScreen";
import { FeePoliciesScreen } from "@/domains/config/FeePoliciesScreen";
import { InstallmentTemplatesScreen } from "@/domains/config/InstallmentTemplatesScreen";
import { DiscountsScreen } from "@/domains/config/DiscountsScreen";
import { DiscountScreen } from "@/domains/config/DiscountScreen";
import { ReferenceScreen } from "@/domains/config/ReferenceScreen";
import { BulkAccountsScreen } from "@/domains/bulk/BulkAccountsScreen";
import { BulkPromotionsScreen } from "@/domains/bulk/BulkPromotionsScreen";
import { ImportsScreen } from "@/domains/imports/ImportsScreen";
import { ImportScreen } from "@/domains/imports/ImportScreen";
import { OperatorsScreen } from "@/domains/admin/OperatorsScreen";
import { OperatorScreen } from "@/domains/admin/OperatorScreen";
import { BackupScreen } from "@/domains/admin/BackupScreen";
import { DataScreen } from "@/domains/admin/DataScreen";
import { SettingsScreen } from "@/domains/admin/SettingsScreen";
import { ReportsScreen } from "@/domains/reports/ReportsScreen";
import { KeyedReportScreen } from "@/domains/reports/ReportScreen";
import { ReconciliationScreen } from "@/domains/oversight/ReconciliationScreen";
import { AuditChainScreen } from "@/domains/oversight/AuditChainScreen";
import { VoidLogScreen } from "@/domains/oversight/VoidLogScreen";
import { YearsScreen } from "@/domains/years/YearsScreen";
import { YearScreen } from "@/domains/years/YearScreen";
import { NewYearScreen } from "@/domains/years/NewYearScreen";
import { HostingScreen } from "@/domains/hosting/HostingScreen";
import { IdentityScreen } from "@/domains/students/IdentityScreen";
import { StatementScreen } from "@/domains/students/StatementScreen";
import { MySessionsScreen } from "@/domains/auth/MySessionsScreen";
import { NotBuiltScreen } from "@/components/NotBuiltScreen";
import { UnavailableScreen } from "@/components/UnavailableScreen";

import { CommandPalette } from "./CommandPalette";
import { ContextBar } from "./ContextBar";
import { Nav } from "./Nav";
import { useSession } from "./session";
import { WorkingContextProvider } from "./working-context";
import { HomeScreen } from "./HomeScreen";

export function App() {
  const { user, ready, loading } = useSession();
  const [paletteOpen, setPaletteOpen] = useState(false);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      // event.code, not event.key: on an Arabic layout the K key reports
      // key="ن", and a shortcut compared against "k" is simply dead. The
      // physical position is layout-independent.
      if ((event.ctrlKey || event.metaKey) && event.code === "KeyK") {
        event.preventDefault();
        setPaletteOpen((open) => !open);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  if (loading || !ready) {
    return (
      <div className="login">
        <p className="note">…</p>
      </div>
    );
  }

  if (!user) return <SignInScreen />;

  // A temporary password is its own mode: every other route refuses the
  // operator until it is changed, so sending them anywhere else would show a
  // screen full of refusals instead of the one form that resolves them.
  if (user.must_change_password) return <ChangePasswordScreen />;

  return (
    <WorkingContextProvider>
      <div className="shell">
        <Nav />
        <div className="shell__main">
          <ContextBar />
          <Routes>
            <Route path="/" element={<HomeScreen />} />

            {/* Work */}
            <Route path="/desk" element={<DeskScreen />} />
            <Route path="/inbox/voids" element={<VoidInboxScreen />} />
            <Route path="/inbox/refunds" element={<RefundInboxScreen />} />
            <Route
              path="/inbox/discounts"
              element={
                <UnavailableScreen
                  title="خصومات تنتظر تأكيد الأهلية"
                  what="أمر تأكيد الأهلية موجود ويعمل على تطبيق بعينه، لكن لا يوجد مسار يسرد التطبيقات المعلّقة عبر الطلبة."
                  missing="مسار مثل GET /discounts/applications?status=pending_confirmation"
                  workaround={{
                    label: "افتح ملف طالب لرؤية خصوماته",
                    to: "/students",
                  }}
                />
              }
            />
            <Route
              path="/inbox/shifts"
              element={
                <UnavailableScreen
                  title="ورديات تنتظر اعتماداً"
                  what="اعتماد الوردية موجود ويعمل على وردية بعينها، لكن لا يوجد مسار يسرد الورديات المغلقة التي تنتظر اعتماداً."
                  missing="مسار مثل GET /cashier-sessions?status=closed"
                  workaround={{ label: "كشف الوردية الحالية", to: "/desk/sessions" }}
                />
              }
            />

            {/* Records */}
            <Route path="/students" element={<StudentSearchScreen />} />
            <Route path="/students/:id/identity" element={<IdentityScreen />} />
            <Route path="/students/:id/statement" element={<StatementScreen />} />
            <Route path="/students/new" element={<RegisterStudentScreen />} />
            <Route path="/students/:id/edit" element={<EditStudentScreen />} />
            <Route path="/students/:id" element={<StudentScreen />} />
            <Route path="/students/:id/discounts/new" element={<GrantDiscountScreen />} />
            <Route path="/enrollments/new" element={<EnrollScreen />} />
            <Route path="/enrollments/:id" element={<EnrollmentScreen />} />
            <Route path="/accounts/new" element={<GenerateAccountScreen />} />
            <Route path="/accounts/:id" element={<AccountScreen />} />
            <Route path="/payments/:id" element={<PaymentScreen />} />
            <Route path="/hosting" element={<HostingScreen />} />
            <Route path="/me/sessions" element={<MySessionsScreen />} />
            {/*
              Reachable on purpose, not only when the server forces it. The
              screen was mounted solely behind must_change_password, so an
              operator who wanted to change a password nobody had reset for
              them had nowhere to go — which is how one credential ends up
              serving an office for years.
            */}
            <Route path="/me/password" element={<ChangePasswordScreen />} />
            <Route path="/years" element={<YearsScreen />} />
            <Route path="/years/new" element={<NewYearScreen />} />
            <Route path="/years/:id" element={<YearScreen />} />
            <Route path="/imports" element={<ImportsScreen />} />
            <Route path="/imports/:id" element={<ImportScreen />} />

            {/* Bulk */}
            <Route path="/bulk/accounts" element={<BulkAccountsScreen />} />
            <Route path="/bulk/promotions" element={<BulkPromotionsScreen />} />

            {/* Financial configuration */}
            <Route path="/config/fee-policies" element={<FeePoliciesScreen />} />
            <Route
              path="/config/installment-templates"
              element={<InstallmentTemplatesScreen />}
            />
            <Route path="/config/discounts" element={<DiscountsScreen />} />
            <Route path="/config/discounts/:id" element={<DiscountScreen />} />
            <Route path="/config/reference" element={<ReferenceScreen />} />

            {/* Governance */}
            <Route path="/reports" element={<ReportsScreen />} />
            {/* Keyed by the report, so moving between two reports remounts
                rather than carrying the previous one's filters into a screen
                that may not even offer them. */}
            <Route path="/reports/:report" element={<KeyedReportScreen />} />
            <Route path="/oversight/reconciliation" element={<ReconciliationScreen />} />
            <Route path="/oversight/audit" element={<AuditChainScreen />} />
            <Route path="/oversight/voids" element={<VoidLogScreen />} />
            <Route path="/admin/operators" element={<OperatorsScreen />} />
            <Route path="/admin/operators/:id" element={<OperatorScreen />} />
            <Route path="/admin/backups" element={<BackupScreen />} />
            <Route path="/admin/data" element={<DataScreen />} />
            <Route path="/settings" element={<SettingsScreen />} />

            <Route path="*" element={<NotBuiltScreen />} />
          </Routes>
        </div>
      </div>
      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />
    </WorkingContextProvider>
  );
}

export { Navigate };
