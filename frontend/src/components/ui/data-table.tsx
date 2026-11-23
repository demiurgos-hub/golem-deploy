import { Fragment, useMemo, useState, type ReactNode } from "react";
import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";
import { cn } from "@/lib/utils";

export interface Column<T> {
  key: string;
  header: string;
  cell: (item: T) => ReactNode;
  /** Makes the column sortable by this value. */
  sortValue?: (item: T) => string | number;
  className?: string;
  /** Hidden below the md breakpoint to keep narrow screens readable. */
  hideOnSmall?: boolean;
}

interface DataTableProps<T> {
  label: string;
  data: readonly T[];
  columns: Column<T>[];
  rowKey: (item: T) => string | number;
  actions?: (item: T) => ReactNode;
  /** Extra content rendered under a row, spanning all columns (e.g. an inline job). */
  rowDetail?: (item: T) => ReactNode;
  initialSort?: { key: string; asc: boolean };
}

export function DataTable<T>({ label, data, columns, rowKey, actions, rowDetail, initialSort }: DataTableProps<T>) {
  const [sort, setSort] = useState<{ key: string; asc: boolean } | null>(initialSort ?? null);

  const sorted = useMemo(() => {
    if (!sort) return data;
    const col = columns.find((c) => c.key === sort.key);
    const get = col?.sortValue;
    if (!get) return data;
    return [...data].sort((a, b) => {
      const va = get(a);
      const vb = get(b);
      const cmp = typeof va === "number" && typeof vb === "number" ? va - vb : String(va).localeCompare(String(vb));
      return sort.asc ? cmp : -cmp;
    });
  }, [data, sort, columns]);

  const colCount = columns.length + (actions ? 1 : 0);

  return (
    <div className="min-w-0 overflow-x-auto rounded-md border border-border">
      <table className="w-full text-sm">
        <caption className="sr-only">{label}</caption>
        <thead>
          <tr className="border-b border-border bg-muted/50">
            {columns.map((col) => {
              const active = sort?.key === col.key;
              return (
                <th
                  key={col.key}
                  scope="col"
                  aria-sort={active ? (sort.asc ? "ascending" : "descending") : undefined}
                  className={cn(
                    "whitespace-nowrap px-3 py-2 text-left font-medium text-muted-foreground",
                    col.hideOnSmall && "hidden md:table-cell",
                    col.className,
                  )}
                >
                  {col.sortValue ? (
                    <button
                      type="button"
                      className="inline-flex items-center gap-1 rounded-sm hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                      onClick={() => setSort(active ? { key: col.key, asc: !sort.asc } : { key: col.key, asc: true })}
                    >
                      {col.header}
                      {active ? (
                        sort.asc ? <ArrowUp className="size-3" /> : <ArrowDown className="size-3" />
                      ) : (
                        <ArrowUpDown className="size-3 opacity-40" />
                      )}
                    </button>
                  ) : (
                    col.header
                  )}
                </th>
              );
            })}
            {actions && (
              <th scope="col" className="whitespace-nowrap px-3 py-2 text-right font-medium text-muted-foreground">
                <span className="sr-only">Actions</span>
              </th>
            )}
          </tr>
        </thead>
        <tbody>
          {sorted.map((item) => {
            const detail = rowDetail?.(item);
            return (
              <Fragment key={rowKey(item)}>
                <tr className={cn("border-border hover:bg-muted/30", !detail && "border-b last:border-0")}>
                  {columns.map((col) => (
                    <td
                      key={col.key}
                      className={cn("px-3 py-2 align-middle", col.hideOnSmall && "hidden md:table-cell", col.className)}
                    >
                      {col.cell(item)}
                    </td>
                  ))}
                  {actions && (
                    <td className="px-3 py-2 text-right align-middle">
                      <div className="flex items-center justify-end gap-1.5">{actions(item)}</div>
                    </td>
                  )}
                </tr>
                {detail && (
                  <tr className="border-b border-border last:border-0">
                    <td colSpan={colCount} className="px-3 pb-3 pt-0">
                      {detail}
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
