"use client";

import { useTaskElicitations, type ElicitationChoice } from "@multica/core/agents";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

export function TaskApprovalPanel({ taskId, active }: { taskId: string; active: boolean }) {
  const { t } = useT("agents");
  const { query, respond } = useTaskElicitations(useWorkspaceId(), taskId, active);
  if (!active) return null;
  if (query.error) {
    // Older servers have no route; other viewers cannot approve this device.
    if (query.error instanceof ApiError && [403, 404].includes(query.error.status)) return null;
    return <p role="alert" className="px-4 py-3 text-caption text-destructive">{t(($) => $.approval.load_error)}</p>;
  }
  if (!query.data?.length) return null;
  const labels: Record<ElicitationChoice, string> = {
    once: t(($) => $.approval.once),
    session: t(($) => $.approval.session),
    always: t(($) => $.approval.always),
    decline: t(($) => $.approval.decline),
    cancel: t(($) => $.approval.cancel),
  };
  return (
    <section aria-label={t(($) => $.approval.title)} className="max-h-80 shrink-0 overflow-y-auto border-b bg-muted/20 px-4 py-3">
      <h3 className="text-body font-medium">{t(($) => $.approval.title)}</h3>
      {query.data.map(({ id, request }) => {
        const choices: ElicitationChoice[] = ["once"];
        if (request._meta.persist.includes("session")) choices.push("session");
        if (request._meta.persist.includes("always")) choices.push("always");
        choices.push("decline", "cancel");
        const warning = request._meta.warningSubtitle || request._meta.subtitle;
        return (
          <div key={id} className="mt-3 space-y-2">
            <p className="whitespace-pre-wrap break-words text-body">{request.message}</p>
            <p className="break-all text-caption text-muted-foreground">{request.serverName} · {request._meta.origin ?? request._meta.tool_params?.app}</p>
            {warning && <p className="whitespace-pre-wrap text-caption">{warning}</p>}
            <div className="flex flex-wrap gap-2">
              {choices.map((choice) => <Button key={choice} size="sm" variant={choice === "once" ? "default" : "outline"} disabled={respond.isPending} aria-busy={respond.isPending} onClick={() => respond.mutate({ id, choice })}>{labels[choice]}</Button>)}
            </div>
            <details className="text-caption text-muted-foreground">
              <summary>{t(($) => $.approval.details)}</summary>
              <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-all">{JSON.stringify(request, null, 2)}</pre>
            </details>
          </div>
        );
      })}
      {respond.isError && <p role="alert" className="mt-2 text-caption text-destructive">{t(($) => $.approval.response_error)}</p>}
    </section>
  );
}
