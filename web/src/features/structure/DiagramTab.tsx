import { lazy, Suspense } from "react";
import type { Tab } from "../../lib/store";
import type { Connection } from "../../lib/types";
import { Spinner } from "../../components/ui";

const DiagramCanvas = lazy(() => import("./DiagramCanvas"));

export function DiagramTab({ tab, conn }: { tab: Tab; conn: Connection }) {
  return (
    <Suspense fallback={<div className="structure__center"><Spinner large /></div>}>
      <DiagramCanvas conn={conn} database={tab.database} schema={tab.schema} />
    </Suspense>
  );
}
