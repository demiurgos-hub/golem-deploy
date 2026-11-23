import { useState, useMemo, useCallback } from "react";
import {
  ArrowUp,
  ArrowDown,
  ArrowUpDown,
  Check,
  Square,
  CheckSquare,
} from "lucide-react";
import { cn } from "@/lib/utils";

export interface Column<T> {
  key: string;
  label: string;
  sortable?: boolean;
  render: (item: T) => React.ReactNode;
  getValue?: (item: T) => string | number;
}

interface DataTableProps<T> {
  data: T[];
  columns: Column<T>[];
  getRowId: (item: T) => string;
  actions?: (item: T) => React.ReactNode;
  selectable?: boolean;
  selectedIds?: Set<string>;
  onSelectionChange?: (ids: Set<string>) => void;
  emptyMessage?: string;
}

export function DataTable<T>({
  data,
  columns,
  getRowId,
  actions,
  selectable = false,
  selectedIds,
  onSelectionChange,
  emptyMessage = "No data",
}: DataTableProps<T>) {
  const [sortKey, setSortKey] = useState<string | null>(null);
  const [sortAsc, setSortAsc] = useState(true);

  const toggleSort = useCallback(
    (key: string) => {
      if (sortKey === key) {
        setSortAsc((a) => !a);
      } else {
        setSortKey(key);
        setSortAsc(true);
      }
    },
    [sortKey],
  );

  const sorted = useMemo(() => {
    if (!sortKey) return data;
    const col = columns.find((c) => c.key === sortKey);
    if (!col?.getValue) return data;
    const getValue = col.getValue;
    return [...data].sort((a, b) => {
      const va = getValue(a);
      const vb = getValue(b);
      const cmp = typeof va === "number" && typeof vb === "number"
        ? va - vb
        : String(va).localeCompare(String(vb));
      return sortAsc ? cmp : -cmp;
    });
  }, [data, sortKey, sortAsc, columns]);

  const allSelected =
    selectable && data.length > 0 && selectedIds?.size === data.length;

  const toggleAll = () => {
    if (!onSelectionChange) return;
    if (allSelected) {
      onSelectionChange(new Set());
    } else {
      onSelectionChange(new Set(data.map(getRowId)));
    }
  };

  const toggleOne = (id: string) => {
    if (!onSelectionChange || !selectedIds) return;
    const next = new Set(selectedIds);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    onSelectionChange(next);
  };

  const colCount =
    columns.length + (selectable ? 1 : 0) + (actions ? 1 : 0) + 1;

  return (
    <div className="min-w-0 overflow-auto rounded-md border border-border">
      <table className="w-full text-sm">
        <colgroup>
          {selectable && <col className="w-10" />}
          {columns.map((col) => (
            <col key={col.key} />
          ))}
          {actions && <col />}
          <col className="w-full" />
        </colgroup>
        <thead>
          <tr className="border-b border-border bg-muted/50">
            {selectable && (
              <th className="w-10 px-3 py-2">
                <button onClick={toggleAll} className="text-muted-foreground hover:text-foreground">
                  {allSelected ? (
                    <CheckSquare className="h-4 w-4" />
                  ) : (
                    <Square className="h-4 w-4" />
                  )}
                </button>
              </th>
            )}
            {columns.map((col) => (
              <th
                key={col.key}
                className={cn(
                  "whitespace-nowrap px-3 py-2 text-left font-medium text-muted-foreground",
                  col.sortable && "cursor-pointer select-none hover:text-foreground",
                )}
                onClick={col.sortable ? () => toggleSort(col.key) : undefined}
              >
                <span className="flex items-center gap-1">
                  {col.label}
                  {col.sortable &&
                    (sortKey === col.key ? (
                      sortAsc ? (
                        <ArrowUp className="h-3 w-3" />
                      ) : (
                        <ArrowDown className="h-3 w-3" />
                      )
                    ) : (
                      <ArrowUpDown className="h-3 w-3 opacity-30" />
                    ))}
                </span>
              </th>
            ))}
            {actions && (
              <th className="whitespace-nowrap px-3 py-2 text-left font-medium text-muted-foreground">
                Actions
              </th>
            )}
            <th aria-hidden className="w-full p-0 py-2" />
          </tr>
        </thead>
        <tbody>
          {sorted.length === 0 ? (
            <tr>
              <td
                colSpan={colCount}
                className="px-3 py-8 text-center text-muted-foreground"
              >
                {emptyMessage}
              </td>
            </tr>
          ) : (
            sorted.map((item) => {
              const id = getRowId(item);
              const selected = selectedIds?.has(id);
              return (
                <tr
                  key={id}
                  className={cn(
                    "border-b border-border last:border-0 hover:bg-muted/30",
                    selected && "bg-primary/5",
                  )}
                >
                  {selectable && (
                    <td className="px-3 py-2">
                      <button
                        onClick={() => toggleOne(id)}
                        className="text-muted-foreground hover:text-foreground"
                      >
                        {selected ? (
                          <Check className="h-4 w-4" />
                        ) : (
                          <Square className="h-4 w-4" />
                        )}
                      </button>
                    </td>
                  )}
                  {columns.map((col) => (
                    <td
                      key={col.key}
                      className="whitespace-nowrap px-3 py-2 text-foreground"
                    >
                      {col.render(item)}
                    </td>
                  ))}
                  {actions && (
                    <td className="whitespace-nowrap px-3 py-2 text-foreground">
                      <div className="flex items-center justify-start gap-[0.3rem] [&_button]:px-[0.6rem] [&_button]:py-[0.3rem] [&_svg]:size-[0.9rem]">
                        {actions(item)}
                      </div>
                    </td>
                  )}
                  <td aria-hidden className="w-full p-0" />
                </tr>
              );
            })
          )}
        </tbody>
      </table>
    </div>
  );
}
