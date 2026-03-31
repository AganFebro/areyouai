import { AdminDashboard } from "@/components/admin-dashboard";

export default function AdminPage() {
  return (
    <main style={{ maxWidth: 1200, margin: "0 auto", padding: "32px 20px 48px" }}>
      <h1 style={{ marginTop: 0, fontSize: "2rem" }}>Admin Dashboard</h1>
      <p style={{ color: "#94a3b8" }}>
        Overview, room operations visibility, and audit timeline.
      </p>
      <AdminDashboard />
    </main>
  );
}

