// The screen table (design §2.2): nav group, minimum role, whether the sample panel shows it, and its lazy chunk.
import { lazy, type ComponentType } from "react";
import type { Route } from "./router";
import type { Role, User } from "./types";

export interface Screen {
  id: string;
  /** English i18n key: nav label, page title and document title. */
  title: string;
  group?: "Operations" | "Administration";
  role: Role;
  demo: boolean;
  /** Undefined while the screen is not built yet (phases 3-4): a placeholder shows. */
  component?: ComponentType<{ route: Route }>;
}

export const screens: Screen[] = [
  { id: "overview", title: "Overview", group: "Operations", role: "viewer", demo: true, component: lazy(() => import("./screens/overview/Overview")) },
  { id: "jobs", title: "Jobs", group: "Operations", role: "viewer", demo: true, component: lazy(() => import("./screens/jobs/Jobs")) },
  { id: "runners", title: "Runners", group: "Operations", role: "viewer", demo: true, component: lazy(() => import("./screens/runners/Runners")) },
  { id: "hosts", title: "Hosts", group: "Operations", role: "viewer", demo: true, component: lazy(() => import("./screens/hosts/Hosts")) },
  { id: "usage", title: "Usage", group: "Operations", role: "viewer", demo: true, component: lazy(() => import("./screens/usage/Usage")) },
  { id: "settings", title: "Settings", group: "Administration", role: "admin", demo: false },
  { id: "team", title: "Team", group: "Administration", role: "admin", demo: false },
  { id: "audit", title: "Audit log", group: "Administration", role: "admin", demo: false },
  { id: "backups", title: "Backups", group: "Administration", role: "admin", demo: true },
  { id: "migrate", title: "Workflow migration", group: "Administration", role: "admin", demo: true },
  // In the user menu, not the nav.
  { id: "tokens", title: "API tokens", role: "viewer", demo: false },
];

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

export const hasRole = (user: User | null, role: Role) => !!user && rank[user.role] >= rank[role];

export const visibleScreens = (user: User | null, demo: boolean) =>
  screens.filter((screen) => (demo ? screen.demo : hasRole(user, screen.role)));
