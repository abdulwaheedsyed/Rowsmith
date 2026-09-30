import { ExportDialog } from "./ExportDialog";
import { ImportDialog } from "./ImportDialog";
import { useTransfer } from "./store";

/** Mounted once by the shell; opens whichever transfer was requested. */
export function TransferDialogs() {
  const { req, set } = useTransfer();
  if (!req) return null;
  const close = () => set(null);
  return req.type === "export"
    ? <ExportDialog key={JSON.stringify(req.source)} conn={req.conn} source={req.source} onClose={close} />
    : <ImportDialog key={JSON.stringify(req.target)} conn={req.conn} target={req.target} onClose={close} />;
}
