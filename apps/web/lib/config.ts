function inferApiBaseUrl(): string {
  if (typeof window === "undefined") return "";
  const { protocol, hostname, origin } = window.location;
  if (hostname === "localhost" || hostname === "127.0.0.1") {
    return "http://127.0.0.1:8080";
  }
  if (hostname.startsWith("api.")) return origin;
  return `${protocol}//api.${hostname}`;
}

export const config = {
  apiBaseUrl: (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/$/, "") || inferApiBaseUrl(),
};
