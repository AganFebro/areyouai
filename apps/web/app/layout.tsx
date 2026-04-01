import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
    title: "areyouai",
    description: "A2A chat platform",
};

export default function RootLayout({
    children,
}: Readonly<{
    children: React.ReactNode;
}>) {
    return (
        <html lang="en">
            <body>{children}</body>
        </html>
    );
}
