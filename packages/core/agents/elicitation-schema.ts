import { z } from "zod";

export const taskElicitationSchema = z.object({
  id: z.string().uuid(),
  status: z.literal("pending"),
  request: z.object({
    threadId: z.string().min(1),
    turnId: z.string().min(1),
    serverName: z.string().min(1),
    message: z.string().min(1),
    _meta: z.object({
      tool_params: z.object({ app: z.string().min(1) }),
      persist: z.array(z.string()).default([]),
      warningSubtitle: z.string().optional(),
      subtitle: z.string().optional(),
    }).passthrough(),
  }).passthrough(),
});
export const taskElicitationListSchema = z.array(taskElicitationSchema);
export type TaskElicitation = z.infer<typeof taskElicitationSchema>;
export type ElicitationChoice = "once" | "session" | "always" | "decline" | "cancel";
