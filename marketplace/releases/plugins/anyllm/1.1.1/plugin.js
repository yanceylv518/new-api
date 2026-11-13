const MODELS = {
  "doubao-seedance-2-0-260128": ["480p", "720p", "1080p", "4k"],
  "doubao-seedance-2-0-fast-260128": ["480p", "720p"],
  "doubao-seedance-2-0-mini-260615": ["480p", "720p"],
  "doubao-seedance-2-5-260628": ["480p", "720p", "1080p"],
};
const OPTIONS = [
  "omni_reference_task_type",
  "ratio",
  "generate_audio",
  "watermark",
  "output_format",
  "return_last_frame",
  "execution_expires_after",
  "priority",
  "tools",
];
const PIXELS = { "480p": 864 * 480, "720p": 1280 * 720, "1080p": 1920 * 1080, "4k": 3840 * 2160 };

function schema(resolutions) {
  return {
    tokens: { type: "number", unit: "token", description: { en: "Billing token unit price", zh: "计费 Token 单价" } },
    resolution: { enum: resolutions, description: { en: "Output video resolution", zh: "输出视频分辨率" } },
    video_input: {
      enum: ["none", "video"],
      enumLabels: { none: { en: "No reference video", zh: "无参考视频" }, video: { en: "With reference video", zh: "有参考视频" } },
      description: { en: "Reference video input", zh: "参考视频输入" },
    },
  };
}

function examples(resolutions) {
  return resolutions.map((resolution) => ({
    label: resolution + " · 5s",
    facts: { tokens: Math.ceil((5 * PIXELS[resolution] * 24) / 1024), resolution, video_input: "none" },
  }));
}

export const meta = {
  apiVersion: 1,
  key: "anyllm",
  name: "Doubao Video（AnyLLM）",
  icon: "text:SD",
  version: "1.1.1",
  author: { name: "PinduCloud" },
  description: { en: "Seedance video generation and private assets", zh: "Seedance 视频生成与私域素材" },
  requiredCapabilities: ["shared-native-routes@1", "poll-task-data@1"],
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  usageSchema: schema(["480p", "720p", "1080p", "4k"]),
  usageExamples: examples(["480p", "720p", "1080p", "4k"]),
  usageProfiles: Object.keys(MODELS).map((model) => ({ models: [model], schema: schema(MODELS[model]), examples: examples(MODELS[model]) })),
  routes: [
    { method: "POST", path: "/doubao/api/v3/contents/generations/tasks", protocol: "seedance-v3", type: "submit", decode: "createTask", render: "taskCreated" },
    { method: "GET", path: "/doubao/api/v3/contents/generations/tasks/:task_id", protocol: "seedance-v3", type: "query", render: "taskStatus" },
    {
      method: "GET",
      path: "/doubao/api/v3/contents/generations/tasks",
      protocol: "seedance-v3",
      type: "dynamic",
      action: "list",
      decode: "listTasks",
      render: "taskList",
    },
    {
      method: "DELETE",
      path: "/doubao/api/v3/contents/generations/tasks/:task_id",
      protocol: "seedance-v3",
      type: "dynamic",
      action: "delete",
      decode: "deleteTask",
      render: "taskDeleted",
    },
  ],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

function object(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(label + " must be an object");
  return value;
}

function text(value) {
  return typeof value === "string" ? value.trim() : "";
}

function integer(value, min, max, label) {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < min || value > max) throw new Error(label + " is out of range");
  return value;
}

function apiRoot(ctx) {
  const base = text(ctx.baseUrl).replace(/\/+$/, "");
  return /\/v1$/i.test(base) ? base : base + "/v1";
}

function mediaURL(value) {
  if (typeof value !== "string" || !/^https?:\/\/[^\s]+$/i.test(value)) throw new Error("Reference media must use a public HTTP(S) URL");
  return value;
}

function referenceURL(value) {
  // asset:// 的归属与目标账号映射由宿主统一完成；下载仍只允许公网 URL。
  if (typeof value === "string" && /^asset:\/\/[a-zA-Z0-9_-]{1,128}$/i.test(value)) return value;
  return mediaURL(value);
}

function modelProfile(ctx, requestedModel) {
  const model = text(ctx.upstreamModel) || text(ctx.model) || text(requestedModel);
  if (MODELS[model]) return model;
  if (MODELS[ctx.model]) return ctx.model;
  throw new Error("Unsupported Seedance video model");
}

function normalize(ctx, request) {
  const req = object(request, "request body");
  const metadata = req.metadata === undefined ? {} : object(req.metadata, "metadata");
  const fields = Object.assign({}, metadata, req);
  const model = modelProfile(ctx, req.model);
  const limits = MODELS[model];
  const is25 = model === "doubao-seedance-2-5-260628";
  const maxDuration = is25 ? 30 : 15;
  let duration = fields.seconds === undefined ? fields.duration : fields.seconds;
  if (fields.seconds !== undefined && fields.duration !== undefined && fields.seconds !== fields.duration) throw new Error("seconds and duration must match");
  if (duration !== undefined && duration !== -1) integer(duration, 4, maxDuration, "duration");
  const requestedResolution = fields.resolution === undefined ? fields.size : fields.resolution;
  if (requestedResolution !== undefined && !text(requestedResolution)) throw new Error("resolution must be a non-empty string");
  const resolution = text(requestedResolution).toLowerCase() || "720p";
  if (!limits.includes(resolution)) throw new Error("Unsupported resolution for this Seedance model: " + resolution);
  for (const key of ["draft", "callback_url", "safety_identifier", "frames", "camera_fixed"]) {
    if (Object.prototype.hasOwnProperty.call(fields, key)) throw new Error(key + " is not supported by this video interface");
  }
  if (fields.replyType !== undefined && fields.replyType !== "async") throw new Error("replyType must be async on the task API");
  let content = fields.content;
  if (content === undefined) {
    content = [];
    if (text(fields.prompt)) content.push({ type: "text", text: fields.prompt });
    if (fields.input_reference !== undefined)
      content.push({ type: "image_url", role: "first_frame", image_url: { url: referenceURL(fields.input_reference) } });
  }
  if (!Array.isArray(content) || !content.length || content.length > 128) throw new Error("content must contain 1 to 128 entries");
  let images = 0,
    audios = 0,
    videos = 0,
    firstFrame = false;
  for (const item of content) {
    object(item, "content entry");
    if (item.type === "text") {
      if (!text(item.text)) throw new Error("Text content must not be empty");
    } else if (["image_url", "video_url", "audio_url"].includes(item.type)) {
      referenceURL(object(item[item.type], item.type).url);
      if (item.type === "image_url") images++;
      else if (item.type === "video_url") videos++;
      else audios++;
      if (item.role === "first_frame" || item.role === "last_frame") firstFrame = true;
    } else throw new Error("Unsupported content type: " + String(item.type));
  }
  if (images > (is25 ? 30 : 9) || audios > (is25 ? 10 : 3)) throw new Error("Too many reference media entries");
  const body = { model: text(ctx.model) || text(req.model), content, resolution };
  for (const key of OPTIONS) if (Object.prototype.hasOwnProperty.call(fields, key)) body[key] = fields[key];
  if (body.ratio !== undefined && !["16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"].includes(body.ratio)) throw new Error("Unsupported ratio");
  for (const key of ["generate_audio", "watermark", "return_last_frame"])
    if (body[key] !== undefined && typeof body[key] !== "boolean") throw new Error(key + " must be a boolean");
  if (body.output_format !== undefined && !["mp4", "mov"].includes(body.output_format)) throw new Error("output_format must be mp4 or mov");
  if (body.priority !== undefined) integer(body.priority, 0, 9, "priority");
  if (body.execution_expires_after !== undefined) integer(body.execution_expires_after, 3600, 259200, "execution_expires_after");
  const operation = body.omni_reference_task_type;
  if (operation !== undefined && (!is25 || !["auto", "reference", "edit", "extend"].includes(operation)))
    throw new Error("Unsupported omni_reference_task_type");
  if ((operation === "edit" || operation === "extend") && !videos) throw new Error("Video editing and extension require reference video");
  if (operation === "edit" && duration !== -1) throw new Error("Video editing requires duration -1");
  if (is25 && (firstFrame || operation === "edit" || operation === "extend")) {
    if (body.ratio !== undefined && body.ratio !== "adaptive") throw new Error("This task requires ratio adaptive");
    body.ratio = "adaptive";
  }
  // 智能时长是控制值；规范请求单独保存，不能让 -1 进入通用计费用量校验。
  if (duration === -1) body.automatic_duration = true;
  else if (duration !== undefined) body.duration = duration;
  return body;
}

function decode(ctx, req) {
  const body = normalize(ctx, req);
  return {
    kind: "submit",
    model: body.model,
    action: body.content.some((item) => item.type !== "text") ? "image_to_video" : "text_to_video",
    requestBody: body,
  };
}

function facts(ctx) {
  const req = object(ctx.requestBody, "request body");
  const model = modelProfile(ctx, req.model);
  const maxDuration = model === "doubao-seedance-2-5-260628" ? 30 : 15;
  const duration = req.automatic_duration || req.duration === undefined ? maxDuration : integer(req.duration, 4, maxDuration, "duration");
  const videoInput = req.content.some((item) => item.type === "video_url");
  // 提交仅作预算；参考视频按文档总时长上限预留，完成后使用媒体账单的 totalTokens。
  const seconds = duration + (videoInput ? maxDuration : 0);
  return { tokens: Math.ceil((seconds * PIXELS[req.resolution] * 24) / 1024), resolution: req.resolution, video_input: videoInput ? "video" : "none" };
}

function auth(ctx) {
  return { Accept: "application/json", Authorization: "Bearer " + ctx.apiKey };
}

export function buildSubmitRequest(ctx) {
  const request = object(ctx.requestBody, "request body");
  const body = Object.assign({}, request, { model: ctx.upstreamModel || ctx.model, replyType: "async" });
  if (body.automatic_duration) body.duration = -1;
  delete body.automatic_duration;
  if (!text(ctx.publicTaskId)) throw new Error("Missing gateway task identity");
  return {
    url: apiRoot(ctx) + "/videos/generations",
    method: "POST",
    headers: Object.assign(auth(ctx), { "Content-Type": "application/json", "Idempotency-Key": ctx.publicTaskId }),
    body,
  };
}

export function parseSubmitResponse(ctx, response) {
  const body = object(response.body, "submission response");
  if (!text(body.id)) throw new Error("Video service returned no task ID");
  const state = { facts: facts(ctx), phase: "result", created_at: utils.unixNow() };
  return { taskId: body.id, taskData: taskSnapshot(ctx, "queued", state), state };
}

export function extractUsage(ctx) {
  // 旧按次价格仍按次处理，避免把 token 预算乘入按次费用。
  return ctx.usagePurpose === "billing_ratios" ? null : facts(ctx);
}

export function buildQueryRequest(ctx) {
  const phase = (ctx.state || {}).phase;
  const path = phase === "usage" ? "/mediaTaskInfo?taskId=" + encodeURIComponent(ctx.taskId) : "/videos/" + encodeURIComponent(ctx.taskId) + "/result";
  // 不跟随响应中的 result_url，查询始终使用管理员配置的同一服务地址。
  return { url: apiRoot(ctx) + path, method: "GET", headers: auth(ctx) };
}

export function parseTaskResult(ctx, raw) {
  const body = object(raw, "task response");
  const state = object(ctx.state, "persisted task state");
  if (state.phase === "usage") {
    if (body.code !== 0) throw new Error("Video usage query failed");
    const data = object(body.data, "media task data");
    if (["pending", "running", "processing"].includes(data.status))
      return { status: "IN_PROGRESS", progress: "95%", taskData: taskSnapshot(ctx, "running", state) };
    if (data.status !== "success") throw new Error("Video usage has an unrecognized or conflicting status");
    const usage = object(data.usage, "media task usage");
    const tokens = integer(usage.totalTokens, 0, Number.MAX_SAFE_INTEGER, "totalTokens");
    // 保留原视频响应作为最终快照；用量就绪后再读一次结果，不改宿主的数据持久化约定。
    return {
      status: "IN_PROGRESS",
      progress: "99%",
      taskData: taskSnapshot(ctx, "running", state),
      state: Object.assign({}, state, { phase: "final", facts: Object.assign({}, state.facts, { tokens }) }),
    };
  }
  if (body.status === "failed") {
    if (state.phase === "final") throw new Error("Video result conflicts with its completed usage record");
    return { status: "FAILURE", progress: "100%", reason: "Video generation failed", taskData: taskSnapshot(ctx, "failed", state) };
  }
  if (body.status === "running" || body.status === "queued" || body.status === "pending")
    return { status: "IN_PROGRESS", progress: "50%", taskData: taskSnapshot(ctx, "running", state) };
  if (body.status !== "succeeded") return { status: "UNKNOWN", reason: "Video task status is unavailable" };
  mediaURL(body.download_url);
  if (state.phase !== "final")
    return { status: "IN_PROGRESS", progress: "95%", taskData: taskSnapshot(ctx, "running", state), state: Object.assign({}, state, { phase: "usage" }) };
  const completed = Object.assign({}, state, { media: { video: body.download_url, last_frame: text(body.last_frame_url) } });
  return {
    status: "SUCCESS",
    progress: "100%",
    url: "/v1/videos/" + encodeURIComponent(ctx.publicTaskId) + "/content",
    totalTokens: state.facts.tokens,
    taskData: taskSnapshot(ctx, "succeeded", completed),
    state: completed,
  };
}

function taskSnapshot(ctx, status, state) {
  const body = { id: ctx.publicTaskId, model: ctx.model, status, created_at: state.created_at, updated_at: utils.unixNow() };
  if (status === "succeeded") {
    body.content = { video_url: "/v1/videos/" + encodeURIComponent(ctx.publicTaskId) + "/content" };
    body.usage = { total_tokens: state.facts.tokens };
  }
  if (status === "failed") body.error = { code: "video_generation_failed", message: "Video generation failed" };
  return body;
}

export function extractUsageOnComplete(task, result) {
  return result.status === "SUCCESS" && task.state && task.state.phase === "final" ? task.state.facts : null;
}

export function publicErrorMessage(error) {
  if (error.local) return error.message;
  if (error.httpStatus === 429) return "Video service is busy; retry later";
  if (error.httpStatus === 400) return "Video request rejected; check the model, media, and parameters";
  if (error.httpStatus === 404) return "Video task is unavailable";
  return "Video request failed";
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS" || !task.state || !task.state.media || !text(task.state.media.video)) return [];
  const items = [{ key: "video", type: "video" }];
  if (text(task.state.media.last_frame)) items.push({ key: "last-frame", type: "image", mimeType: "image/jpeg" });
  return items;
}

export function buildContentRequest(ctx) {
  const media = object(ctx.state && ctx.state.media, "task media");
  const url = ctx.artifactKey === "video" ? media.video : ctx.artifactKey === "last-frame" ? media.last_frame : "";
  if (!text(url)) throw new Error("artifact_not_found");
  return { url: mediaURL(url), method: ctx.clientRequest.method, credentialless: true };
}

function videoStatus(_ctx, task) {
  const statuses = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
  const body = {
    id: task.task_id,
    object: "video.generation",
    created: task.created_at,
    status: statuses[task.status] || "unknown",
    result_url: "/v1/videos/" + encodeURIComponent(task.task_id),
    download_url: null,
    error: null,
  };
  if (task.status === "SUCCESS") {
    body.download_url = "/v1/videos/" + encodeURIComponent(task.task_id) + "/content";
  }
  if (task.status === "FAILURE") body.error = { code: "video_generation_failed", message: task.fail_reason || "Video generation failed" };
  return body;
}

export const native = {
  createTask(ctx) {
    if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
    return decode(ctx, ctx.body.value);
  },
  taskCreated(_ctx, task) {
    return { id: task.task_id };
  },
  taskStatus(_ctx, task) {
    return Object.assign({}, task.data, { id: task.task_id });
  },
  listTasks(ctx) {
    const query = ctx.query || {};
    return { kind: "query", model: (query["filter.model"] || [""])[0], taskIds: query["filter.task_ids"] || [] };
  },
  taskList(_ctx, result) {
    return { items: result.items.map((task) => Object.assign({}, task.data, { id: task.task_id })), total: result.total };
  },
  deleteTask(ctx) {
    return { kind: "delete", taskId: ctx.params.task_id };
  },
  taskDeleted() {
    return {};
  },
  error(_ctx, error) {
    return { error: { code: error.code, message: error.httpStatus >= 500 ? "Video request failed" : error.message } };
  },
};

function decodeVideo(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("Video requests require JSON with media URLs");
  return decode(ctx, Object.assign({}, ctx.body.value, { model: ctx.model }));
}

function decodeResponses(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  const req = object(ctx.body.value, "request body");
  if (req.content !== undefined || (req.metadata && req.metadata.content !== undefined)) return decode(ctx, Object.assign({}, req, { model: ctx.model }));
  const content = [];
  if (typeof req.input === "string") content.push({ type: "text", text: req.input });
  else if (Array.isArray(req.input)) {
    for (const message of req.input) {
      const parts = typeof message === "string" ? [{ type: "input_text", text: message }] : message.content;
      if (!Array.isArray(parts)) throw new Error("Response messages require content arrays");
      for (const part of parts) {
        if (part.type === "input_text" || part.type === "output_text") content.push({ type: "text", text: part.text });
        else if (part.type === "input_image") content.push({ type: "image_url", role: "reference_image", image_url: { url: part.image_url } });
        else throw new Error("Unsupported Responses content type");
      }
    }
  } else throw new Error("input must be a string or array");
  return decode(ctx, Object.assign({}, req, { model: ctx.model, content }));
}

function outputText(ctx) {
  const artifact = ctx.artifacts && ctx.artifacts.video;
  if (!artifact || !text(artifact.url)) throw new Error("Video artifact is unavailable");
  return artifact.url;
}

export const protocols = {
  openai_video: { decodeRequest: decodeVideo, render: videoStatus },
  openai_responses: {
    decodeRequest: decodeResponses,
    renderEvents(ctx, task, previous) {
      const state = { status: task.status, progress: task.progress };
      if (task.status === "SUCCESS")
        return { events: previous && previous.status === "SUCCESS" ? [] : [{ type: "output", data: outputText(ctx) }], state, done: true };
      if (task.status === "FAILURE")
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "Video generation failed" }], state, done: true };
      return {
        events:
          previous && previous.status === task.status && previous.progress === task.progress
            ? []
            : [{ type: "progress", message: "Video generation in progress" }],
        state,
        done: false,
      };
    },
    renderFinal(ctx) {
      return {
        output: [
          { type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: outputText(ctx), annotations: [], logprobs: [] }] },
        ],
      };
    },
  },
};
