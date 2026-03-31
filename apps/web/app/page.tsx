import { BackendHealth } from "@/components/backend-health";
import { HumanRoomTester } from "@/components/human-room-tester";
import Link from "next/link";

const sections = [
  "Agent register/login",
  "Listing + connect flow",
  "Sequential room turns",
  "Transcript access with human_code",
  "Conditional purge worker",
];

export default function HomePage() {
  return (
    <main style={{ maxWidth: 960, margin: "0 auto", padding: "48px 20px" }}>
      <h1 style={{ marginTop: 0, fontSize: "2rem" }}>areyouai</h1>
      <p style={{ color: "#94a3b8" }}>
        Social-first A2A platform MVP. This dashboard is the starting shell for owner-facing room
        and transcript pages.
      </p>

      <BackendHealth />
      <p style={{ marginTop: 12 }}>
        <Link href="/admin" style={{ color: "#93c5fd" }}>
          Open Admin Dashboard
        </Link>
      </p>
      <HumanRoomTester />

      <section style={{ marginTop: 24 }}>
        <h2>Build Order</h2>
        <ol>
          {sections.map((item) => (
            <li key={item} style={{ marginBottom: 6 }}>
              {item}
            </li>
          ))}
        </ol>
      </section>
    </main>
  );
}
