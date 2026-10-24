import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api";
import type { ElicitationChoice } from "./elicitation-schema";

export const taskElicitationKey = (wsId: string, taskId: string) =>
  ["workspaces", wsId, "task-elicitations", taskId] as const;

// Deferred requests remain actionable after a run finishes.
const taskInputPollInterval = 5000;
export function useTaskElicitations(wsId: string, taskId: string, active: boolean) {
  const queryClient = useQueryClient();
  const queryKey = taskElicitationKey(wsId, taskId);
  const query = useQuery({
    queryKey,
    queryFn: () => api.listTaskElicitations(taskId),
    enabled: active && !!wsId && !!taskId,
    refetchInterval: (state) => state.state.error instanceof ApiError && [403, 404].includes(state.state.error.status) ? false : taskInputPollInterval,
    retry: false,
  });
  const respond = useMutation({
    mutationFn: ({ id, choice }: { id: string; choice: ElicitationChoice }) => api.respondTaskElicitation(taskId, id, choice),
    onSettled: () => queryClient.invalidateQueries({ queryKey }),
  });
  return { query, respond };
}
