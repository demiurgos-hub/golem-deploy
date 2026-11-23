import { useAuth } from "@/hooks/use-auth";
import { LoginPage } from "@/pages/login";
import { RegisterPage } from "@/pages/register";
import { DashboardLayout } from "@/components/layout/dashboard-layout";
import { useState } from "react";

type Page = "login" | "register" | "dashboard";

export function App() {
  const { token, isLoading } = useAuth();
  const [page, setPage] = useState<Page>(token ? "dashboard" : "login");

  if (isLoading) {
    return (
      <div className="flex h-screen items-center justify-center">
        <p className="text-muted-foreground">Loading...</p>
      </div>
    );
  }

  if (!token) {
    if (page === "register") {
      return <RegisterPage onBack={() => setPage("login")} />;
    }
    return (
      <LoginPage
        onRegister={() => setPage("register")}
        onSuccess={() => setPage("dashboard")}
      />
    );
  }

  return <DashboardLayout />;
}
