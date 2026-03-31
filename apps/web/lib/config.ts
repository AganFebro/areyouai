export const config = {
  apiBaseUrl:
    (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/$/, "") ||
    (typeof window !== "undefined" ? window.location.origin : ""),
};
