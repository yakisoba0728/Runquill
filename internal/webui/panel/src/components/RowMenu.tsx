// A row's action menu in a DataTable cell (design §4.4, §4.6): the same actions as the detail pane footer and the palette.
// Stratum 0.1.1 has no row action column (design §8), so two things are handled here:
// - The menu's popover renders inside the row, and DataTable runs onRowAction for any click that is not on a button or
//   link. Clicks inside the cell stop here, so the menu's padding or the phone sheet never opens the pane.
// - Rows are one roving tab stop. Every trigger starts out of the Tab order; rowMenuTabs, on the table's wrapper, puts
//   only the focused row's trigger in it, right after its row (Tab to the trigger, Shift+Tab back to the row).
import { IconButton, Menu } from "@yakihyuk0728/stratum-ui";
import type { FocusEvent, ReactNode } from "react";

const MoreIcon = () => (
  <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden>
    <circle cx="5" cy="12" r="1.75" /><circle cx="12" cy="12" r="1.75" /><circle cx="19" cy="12" r="1.75" />
  </svg>
);

export function RowMenu({ label, children }: { label: string; children: ReactNode }) {
  return (
    <span className="row-menu" onClick={(event) => event.stopPropagation()}>
      <Menu align="end" trigger={<IconButton label={label} icon={<MoreIcon />} size="sm" tabIndex={-1} data-row-menu="" />}>{children}</Menu>
    </span>
  );
}

/** onFocus for the element around a DataTable with RowMenu cells. */
export function rowMenuTabs(event: FocusEvent<HTMLElement>) {
  const row = (event.target as Element).closest('[role="row"]');
  if (!row) return;
  for (const trigger of event.currentTarget.querySelectorAll<HTMLElement>("[data-row-menu]")) trigger.tabIndex = row.contains(trigger) ? 0 : -1;
}
