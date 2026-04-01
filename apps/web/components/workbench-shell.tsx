import type { ReactNode } from "react";

type WorkbenchShellProps = {
  children: ReactNode;
};

export function WorkbenchShell({ children }: WorkbenchShellProps) {
  return (
    <div className="wb-root">
      <main className="wb-content">{children}</main>
    </div>
  );
}
