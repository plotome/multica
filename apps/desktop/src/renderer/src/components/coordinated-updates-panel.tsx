import { useEffect } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  api,
  type ForkUpdateState,
  type ForkUpdateAction,
} from "@multica/core/api";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "@multica/views/i18n";

const queryKey = ["fork-update-state"];

export function CoordinatedUpdatesPanel() {
  const { t } = useT("settings");
  const client = useQueryClient();
  const query = useQuery({
    queryKey,
    queryFn: () => api.getForkUpdateState(),
    retry: false,
  });
  useEffect(
    () =>
      window.updater?.onForkCheck?.(() => {
        void client.invalidateQueries({ queryKey });
      }),
    [client],
  );
  const mutation = useMutation({
    mutationFn: ({
      state,
      action,
      target,
    }: {
      state: ForkUpdateState;
      action: ForkUpdateAction;
      target?: string;
    }) => api.actOnForkUpdate(state, action, target),
    onSuccess: (state) => client.setQueryData(queryKey, state),
  });
  const text = t(($) => $.desktop.coordinated_updates, { returnObjects: true });
  if (query.isPending)
    return (
      <p role="status" className="text-body text-muted-foreground">
        {text.checking}
      </p>
    );
  if (query.isError)
    return (
      <div role="alert" className="flex flex-col items-start gap-3">
        <p className="text-body text-destructive">{text.unavailable}</p>
        <Button
          variant="outline"
          size="sm"
          onClick={() => void query.refetch()}
        >
          {text.refresh}
        </Button>
      </div>
    );
  const state = query.data;
  if (!state)
    return (
      <p role="alert" className="text-body text-destructive">
        {text.invalid}
      </p>
    );
  if (!("receipt" in state))
    return (
      <p role="status" className="text-body text-muted-foreground">
        {text.none}
      </p>
    );
  const targets = Object.values(state.receipt.targets);
  const complete =
    state.receipt.approved &&
    targets.length > 0 &&
    targets.every((target) => target.phase === "succeeded");
  const phaseLabel = (phase: string, operation?: string) => {
    if (phase === "failed" && operation === "rollback")
      return text.rollback_failed;
    if (phase === "probe") return text.probing;
    if (phase === "rollback-verification") return text.verifying_rollback;
    if (phase === "running-verification") return text.verifying;
    if (phase === "install") return text.installing;
    if (phase === "rollback") return text.rolling_back;
    switch (phase) {
      case "restoring-baseline":
        return text.restoring_baseline;
      case "verifying-baseline":
        return text.verifying_baseline;
      case "prepared":
        return text.prepared;
      case "downloading":
        return text.downloading;
      case "validating":
        return text.validating;
      case "waiting":
        return text.waiting;
      case "installing":
        return text.installing;
      case "verifying":
        return text.verifying;
      case "succeeded":
        return text.succeeded;
      case "failed":
        return text.failed;
      case "rolling-back":
        return text.rolling_back;
      case "verifying-rollback":
        return text.verifying_rollback;
      case "rolled-back":
        return text.rolled_back;
      default:
        return text.pending;
    }
  };
  const send = (action: ForkUpdateAction, target?: string) =>
    mutation.mutate({ state, action, target });
  return (
    <section aria-label={text.title} className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-body font-medium">{text.title}</h2>
        <Button
          variant="outline"
          size="sm"
          onClick={() => void query.refetch()}
          disabled={query.isFetching}
        >
          {text.refresh}
        </Button>
      </div>
      <p className="text-caption text-muted-foreground">
        {t(($) => $.desktop.coordinated_updates.units, {
          count: targets.length,
        })}
      </p>
      {targets.length === 0 && <p role="status">{text.none}</p>}
      {targets.map((target) => {
        const id = target.artifact.id;
        const known = [
          "prepared",
          "downloading",
          "validating",
          "restoring-baseline",
          "verifying-baseline",
          "waiting",
          "installing",
          "verifying",
          "succeeded",
          "failed",
          "rolling-back",
          "verifying-rollback",
          "rolled-back",
        ].includes(target.phase);
        const rollback =
          target.rollback_available &&
          (!target.recovery || target.recovery.status === "prepared") &&
          ["succeeded", "failed", "installing", "verifying"].includes(
            target.phase,
          ) &&
          target.operation === "install";
        return (
          <article key={id} className="rounded-xl border border-border p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h3 className="text-body font-medium">
                  {target.artifact.host} · {id}
                </h3>
                <p className="mt-1 text-caption text-muted-foreground">
                  {text.target}: {target.artifact.version}
                </p>
              </div>
              <span
                role="status"
                className={
                  target.phase === "failed"
                    ? "text-caption text-destructive"
                    : "text-caption text-muted-foreground"
                }
              >
                {phaseLabel(target.phase, target.operation)}
              </span>
            </div>
            <details className="mt-3 text-caption">
              <summary className="cursor-pointer">{text.components}</summary>
              <div className="mt-2 flex flex-col gap-2">
                {Object.keys(target.artifact.component_versions ?? {}).map(
                  (kind) => {
                    const component = target.observation?.components.find(
                      (component) => component.kind === kind,
                    );
                    return (
                      <div
                        key={kind}
                        className="flex flex-wrap justify-between gap-2"
                      >
                        <span>{kind}</span>
                        <span>
                          {text.current}: {component?.version ?? text.pending} ·{" "}
                          {text.target}:{" "}
                          {target.artifact.component_versions?.[kind] ??
                            text.pending}
                        </span>
                      </div>
                    );
                  },
                )}
                {!target.observation && <p>{text.pending}</p>}
                <p className="break-all font-mono text-muted-foreground">
                  {text.target}: {target.artifact.commit}
                </p>
              </div>
            </details>
            {target.error &&
              target.phase !== "failed" &&
              !target.recovery?.candidate_error && (
                <p role="alert" className="mt-3 text-caption text-destructive">
                  {text.preconditions}: {target.error}
                </p>
              )}
            {target.recovery?.candidate_error && (
              <div
                className="mt-3 flex flex-col gap-2 text-caption"
                role="status"
              >
                <p className="text-destructive">
                  {text.candidate_failed}: {target.recovery.candidate_error}
                </p>
                <p>
                  {text.baseline_recovery}:{" "}
                  {target.recovery.status === "restored"
                    ? text.baseline_restored
                    : target.recovery.status === "failed"
                      ? text.baseline_failed
                      : target.recovery.status === "verifying"
                        ? text.verifying_baseline
                        : text.restoring_baseline}
                </p>
                {target.recovery.error && (
                  <p className="text-destructive">{target.recovery.error}</p>
                )}
              </div>
            )}
            {target.phase === "failed" && (
              <div
                role="alert"
                className="mt-3 flex flex-col gap-2 text-caption text-destructive"
              >
                <p>
                  {text.failure}:{" "}
                  {target.failure_stage
                    ? phaseLabel(target.failure_stage)
                    : text.pending}
                </p>
                <p className="whitespace-pre-wrap break-words">
                  {target.error}
                </p>
                <details>
                  <summary className="cursor-pointer">{text.logs}</summary>
                  <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap">
                    {target.log || text.no_logs}
                  </pre>
                </details>
              </div>
            )}
            {(target.phase === "failed" || rollback) && (
              <div className="mt-3 flex flex-wrap gap-2">
                {target.phase === "failed" && known && (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={mutation.isPending}
                    onClick={() => send("retry", id)}
                  >
                    {target.recovery?.status === "failed"
                      ? text.retry_baseline
                      : target.recovery?.status === "restored"
                        ? text.retry_candidate
                        : text.retry}{" "}
                    · {target.artifact.host} · {id}
                  </Button>
                )}
                {rollback && (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={mutation.isPending}
                    onClick={() => send("rollback", id)}
                  >
                    {text.rollback} · {target.artifact.host} · {id}
                  </Button>
                )}
              </div>
            )}
          </article>
        );
      })}
      <p role="status" className="text-caption text-muted-foreground">
        {complete ? text.complete : text.incomplete}
      </p>
      {!state.receipt.approved && targets.length > 0 && (
        <div>
          <Button
            disabled={
              mutation.isPending ||
              !targets.every((target) =>
                ["prepared", "waiting"].includes(target.phase),
              )
            }
            onClick={() => send("approve")}
          >
            {text.approve}
          </Button>
          <p className="mt-2 text-caption text-muted-foreground">
            {text.wait_notice}
          </p>
        </div>
      )}
      {mutation.isError && (
        <p role="alert" className="text-caption text-destructive">
          {mutation.error instanceof Error
            ? mutation.error.message
            : text.unavailable}
        </p>
      )}
    </section>
  );
}
