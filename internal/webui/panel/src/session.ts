// The signed-in user (design §5.5): read once by main.tsx; roles do not change during a session.
import { isSample } from "./api";
import { hasRole } from "./routes";
import type { Role, User } from "./types";

let user: User | null = null;

export function setUser(next: User | null) {
  user = next;
}

/** v1 ctx.can: the role or above. Always false in the sample panel, so it shows no write controls. */
export const can = (role: Role) => !isSample() && hasRole(user, role);
