export namespace acp {
	
	export class AgentConfig {
	    name: string;
	    command: string;
	    args?: string[];
	    env?: Record<string, string>;
	    cwd?: string;
	    enabled: boolean;
	    protocol?: string;
	
	    static createFrom(source: any = {}) {
	        return new AgentConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.command = source["command"];
	        this.args = source["args"];
	        this.env = source["env"];
	        this.cwd = source["cwd"];
	        this.enabled = source["enabled"];
	        this.protocol = source["protocol"];
	    }
	}

}

export namespace approval {
	
	export class Rule {
	    id?: string;
	    tool: string;
	    pattern?: string;
	    action: string;
	    priority?: number;
	
	    static createFrom(source: any = {}) {
	        return new Rule(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.tool = source["tool"];
	        this.pattern = source["pattern"];
	        this.action = source["action"];
	        this.priority = source["priority"];
	    }
	}

}

export namespace bisheng {
	
	export class Msg {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new Msg(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}

}

export namespace cron {
	
	export class Job {
	    id: string;
	    name: string;
	    expr: string;
	    action?: string;
	    script?: string;
	    scriptTimeoutSec?: number;
	    scriptArgs?: string;
	    noAgent?: boolean;
	    skills?: string[];
	    flowId?: string;
	    workspace: string;
	    mode: string;
	    task: string;
	    enabled: boolean;
	    timeoutMin?: number;
	    retryLimit?: number;
	    model?: string;
	    employeeId?: string;
	    positionId?: string;
	    assignRule?: string;
	    lastAssignedIndex?: number;
	    timeWindowEnabled?: boolean;
	    startTime?: string;
	    endTime?: string;
	    failStreak?: number;
	    disabledReason?: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    // Go type: time
	    lastRun?: any;
	    // Go type: time
	    nextRun?: any;
	    // Go type: time
	    nextRetry?: any;
	
	    static createFrom(source: any = {}) {
	        return new Job(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.expr = source["expr"];
	        this.action = source["action"];
	        this.script = source["script"];
	        this.scriptTimeoutSec = source["scriptTimeoutSec"];
	        this.scriptArgs = source["scriptArgs"];
	        this.noAgent = source["noAgent"];
	        this.skills = source["skills"];
	        this.flowId = source["flowId"];
	        this.workspace = source["workspace"];
	        this.mode = source["mode"];
	        this.task = source["task"];
	        this.enabled = source["enabled"];
	        this.timeoutMin = source["timeoutMin"];
	        this.retryLimit = source["retryLimit"];
	        this.model = source["model"];
	        this.employeeId = source["employeeId"];
	        this.positionId = source["positionId"];
	        this.assignRule = source["assignRule"];
	        this.lastAssignedIndex = source["lastAssignedIndex"];
	        this.timeWindowEnabled = source["timeWindowEnabled"];
	        this.startTime = source["startTime"];
	        this.endTime = source["endTime"];
	        this.failStreak = source["failStreak"];
	        this.disabledReason = source["disabledReason"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.lastRun = this.convertValues(source["lastRun"], null);
	        this.nextRun = this.convertValues(source["nextRun"], null);
	        this.nextRetry = this.convertValues(source["nextRetry"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Run {
	    jobId: string;
	    jobName: string;
	    sessionId: string;
	    // Go type: time
	    startedAt: any;
	    status: string;
	    error?: string;
	    attempt?: number;
	    durationMs?: number;
	
	    static createFrom(source: any = {}) {
	        return new Run(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.jobId = source["jobId"];
	        this.jobName = source["jobName"];
	        this.sessionId = source["sessionId"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.status = source["status"];
	        this.error = source["error"];
	        this.attempt = source["attempt"];
	        this.durationMs = source["durationMs"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace desktop {
	
	export class AddExtensionRequest {
	    repo: string;
	
	    static createFrom(source: any = {}) {
	        return new AddExtensionRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.repo = source["repo"];
	    }
	}
	export class AuditEntry {
	    ts: string;
	    employee: string;
	    tool: string;
	    args?: string;
	    decision: string;
	    reason?: string;
	
	    static createFrom(source: any = {}) {
	        return new AuditEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ts = source["ts"];
	        this.employee = source["employee"];
	        this.tool = source["tool"];
	        this.args = source["args"];
	        this.decision = source["decision"];
	        this.reason = source["reason"];
	    }
	}
	export class AutoDevConfig {
	    enabled: boolean;
	    intervalMin?: number;
	    dailyLimit?: number;
	    maxPlanItems?: number;
	    reviewPrompt?: string;
	    devMode?: string;
	    parallel?: number;
	    maxAttempts?: number;
	    autoReview?: boolean;
	    rejectDelete?: boolean;
	    reviewMode?: string;
	    minDeveloping?: number;
	    verifyCommands?: string[];
	
	    static createFrom(source: any = {}) {
	        return new AutoDevConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.intervalMin = source["intervalMin"];
	        this.dailyLimit = source["dailyLimit"];
	        this.maxPlanItems = source["maxPlanItems"];
	        this.reviewPrompt = source["reviewPrompt"];
	        this.devMode = source["devMode"];
	        this.parallel = source["parallel"];
	        this.maxAttempts = source["maxAttempts"];
	        this.autoReview = source["autoReview"];
	        this.rejectDelete = source["rejectDelete"];
	        this.reviewMode = source["reviewMode"];
	        this.minDeveloping = source["minDeveloping"];
	        this.verifyCommands = source["verifyCommands"];
	    }
	}
	export class AvailablePlugin {
	    id: string;
	    name: string;
	    version?: string;
	    description?: string;
	    downloadUrl?: string;
	    installed: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AvailablePlugin(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.version = source["version"];
	        this.description = source["description"];
	        this.downloadUrl = source["downloadUrl"];
	        this.installed = source["installed"];
	    }
	}
	export class ChannelConfig {
	    type: string;
	    name: string;
	    enabled: boolean;
	    appId?: string;
	    appSecret?: string;
	    webhookKey?: string;
	    botId?: string;
	    botSecret?: string;
	    corpId?: string;
	    agentId?: string;
	    corpSecret?: string;
	    token?: string;
	    encodingAesKey?: string;
	    callbackPort?: number;
	    deviceId?: string;
	    apiBase?: string;
	    remoteRelay?: boolean;
	    webhookUrl?: string;
	    publicId?: string;
	    pubSecret?: string;
	    eid?: string;
	    webhookToken?: string;
	    groupHookIds?: string;
	    groupInMap?: Record<string, string>;
	    outTemplate?: string;
	    allowedUsers?: string;
	    employeeId?: string;
	    positionId?: string;
	    assignRule?: string;
	    lastAssignedIndex?: number;
	
	    static createFrom(source: any = {}) {
	        return new ChannelConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.name = source["name"];
	        this.enabled = source["enabled"];
	        this.appId = source["appId"];
	        this.appSecret = source["appSecret"];
	        this.webhookKey = source["webhookKey"];
	        this.botId = source["botId"];
	        this.botSecret = source["botSecret"];
	        this.corpId = source["corpId"];
	        this.agentId = source["agentId"];
	        this.corpSecret = source["corpSecret"];
	        this.token = source["token"];
	        this.encodingAesKey = source["encodingAesKey"];
	        this.callbackPort = source["callbackPort"];
	        this.deviceId = source["deviceId"];
	        this.apiBase = source["apiBase"];
	        this.remoteRelay = source["remoteRelay"];
	        this.webhookUrl = source["webhookUrl"];
	        this.publicId = source["publicId"];
	        this.pubSecret = source["pubSecret"];
	        this.eid = source["eid"];
	        this.webhookToken = source["webhookToken"];
	        this.groupHookIds = source["groupHookIds"];
	        this.groupInMap = source["groupInMap"];
	        this.outTemplate = source["outTemplate"];
	        this.allowedUsers = source["allowedUsers"];
	        this.employeeId = source["employeeId"];
	        this.positionId = source["positionId"];
	        this.assignRule = source["assignRule"];
	        this.lastAssignedIndex = source["lastAssignedIndex"];
	    }
	}
	export class ChannelStatus {
	    type: string;
	    name: string;
	    enabled: boolean;
	    online: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ChannelStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.name = source["name"];
	        this.enabled = source["enabled"];
	        this.online = source["online"];
	    }
	}
	export class CheckpointEntry {
	    id: string;
	    seq: number;
	    time: number;
	    tool: string;
	    path: string;
	    isNew: boolean;
	
	    static createFrom(source: any = {}) {
	        return new CheckpointEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.seq = source["seq"];
	        this.time = source["time"];
	        this.tool = source["tool"];
	        this.path = source["path"];
	        this.isNew = source["isNew"];
	    }
	}
	export class CleanupReport {
	    sessionId: string;
	    title: string;
	    ok: boolean;
	    skipped: boolean;
	    reason: string;
	    docPath: string;
	    analysis: string;
	
	    static createFrom(source: any = {}) {
	        return new CleanupReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sessionId = source["sessionId"];
	        this.title = source["title"];
	        this.ok = source["ok"];
	        this.skipped = source["skipped"];
	        this.reason = source["reason"];
	        this.docPath = source["docPath"];
	        this.analysis = source["analysis"];
	    }
	}
	export class CleanupSummary {
	    total: number;
	    deleted: number;
	    skipped: number;
	    reports: CleanupReport[];
	
	    static createFrom(source: any = {}) {
	        return new CleanupSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total = source["total"];
	        this.deleted = source["deleted"];
	        this.skipped = source["skipped"];
	        this.reports = this.convertValues(source["reports"], CleanupReport);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModelProfile {
	    kind: string;
	    name: string;
	    baseURL: string;
	    apiKey?: string;
	    model: string;
	    disabled?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelProfile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.baseURL = source["baseURL"];
	        this.apiKey = source["apiKey"];
	        this.model = source["model"];
	        this.disabled = source["disabled"];
	    }
	}
	export class TaskSummaryConfig {
	    enabled: boolean;
	    maxLen?: number;
	    silent?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TaskSummaryConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.maxLen = source["maxLen"];
	        this.silent = source["silent"];
	    }
	}
	export class SelfReviewConfig {
	    enabled: boolean;
	    everyN?: number;
	
	    static createFrom(source: any = {}) {
	        return new SelfReviewConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.everyN = source["everyN"];
	    }
	}
	export class Config {
	    baseURL: string;
	    apiKey: string;
	    model: string;
	    workspace: string;
	    autoApprove: boolean;
	    editorPreference?: string;
	    recentProjects?: string[];
	    contextWindow?: number;
	    autoCondensePercent?: number;
	    condenseDisabled?: boolean;
	    vector?: vector.Config;
	    selfReview?: SelfReviewConfig;
	    taskSummary?: TaskSummaryConfig;
	    autoDev?: AutoDevConfig;
	    imageGenBaseURL?: string;
	    imageGenModel?: string;
	    imageGenAPIKey?: string;
	    videoGenBaseURL?: string;
	    videoGenModel?: string;
	    videoGenAPIKey?: string;
	    videoGenMode?: string;
	    videoGenSize?: string;
	    speechBaseURL?: string;
	    speechModel?: string;
	    speechAPIKey?: string;
	    speechVoice?: string;
	    costPerMPrompt?: number;
	    costPerMCompletion?: number;
	    dangerAutoApprove?: boolean;
	    sandbox?: tools.SandboxConfig;
	    wsPanelDir?: string;
	    maxParallel?: number;
	    smtpHost?: string;
	    smtpPort?: number;
	    smtpUser?: string;
	    smtpPass?: string;
	    smtpFrom?: string;
	    notionAPIKey?: string;
	    browserPath?: string;
	    browserDisabled?: boolean;
	    internalBrowser?: boolean;
	    checkpointDisabled?: boolean;
	    taskLogDisabled?: boolean;
	    webhookEnabled?: boolean;
	    webhookPort?: number;
	    verifyDisabled?: boolean;
	    gitAutocommitEnabled?: boolean;
	    blurNotifyDisabled?: boolean;
	    costLimitUSD?: number;
	    globalCostLimitUSD?: number;
	    auxBaseURL?: string;
	    auxAPIKey?: string;
	    auxModel?: string;
	    fallbackBaseURL?: string;
	    fallbackAPIKey?: string;
	    fallbackModel?: string;
	    modelProfiles?: ModelProfile[];
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.baseURL = source["baseURL"];
	        this.apiKey = source["apiKey"];
	        this.model = source["model"];
	        this.workspace = source["workspace"];
	        this.autoApprove = source["autoApprove"];
	        this.editorPreference = source["editorPreference"];
	        this.recentProjects = source["recentProjects"];
	        this.contextWindow = source["contextWindow"];
	        this.autoCondensePercent = source["autoCondensePercent"];
	        this.condenseDisabled = source["condenseDisabled"];
	        this.vector = this.convertValues(source["vector"], vector.Config);
	        this.selfReview = this.convertValues(source["selfReview"], SelfReviewConfig);
	        this.taskSummary = this.convertValues(source["taskSummary"], TaskSummaryConfig);
	        this.autoDev = this.convertValues(source["autoDev"], AutoDevConfig);
	        this.imageGenBaseURL = source["imageGenBaseURL"];
	        this.imageGenModel = source["imageGenModel"];
	        this.imageGenAPIKey = source["imageGenAPIKey"];
	        this.videoGenBaseURL = source["videoGenBaseURL"];
	        this.videoGenModel = source["videoGenModel"];
	        this.videoGenAPIKey = source["videoGenAPIKey"];
	        this.videoGenMode = source["videoGenMode"];
	        this.videoGenSize = source["videoGenSize"];
	        this.speechBaseURL = source["speechBaseURL"];
	        this.speechModel = source["speechModel"];
	        this.speechAPIKey = source["speechAPIKey"];
	        this.speechVoice = source["speechVoice"];
	        this.costPerMPrompt = source["costPerMPrompt"];
	        this.costPerMCompletion = source["costPerMCompletion"];
	        this.dangerAutoApprove = source["dangerAutoApprove"];
	        this.sandbox = this.convertValues(source["sandbox"], tools.SandboxConfig);
	        this.wsPanelDir = source["wsPanelDir"];
	        this.maxParallel = source["maxParallel"];
	        this.smtpHost = source["smtpHost"];
	        this.smtpPort = source["smtpPort"];
	        this.smtpUser = source["smtpUser"];
	        this.smtpPass = source["smtpPass"];
	        this.smtpFrom = source["smtpFrom"];
	        this.notionAPIKey = source["notionAPIKey"];
	        this.browserPath = source["browserPath"];
	        this.browserDisabled = source["browserDisabled"];
	        this.internalBrowser = source["internalBrowser"];
	        this.checkpointDisabled = source["checkpointDisabled"];
	        this.taskLogDisabled = source["taskLogDisabled"];
	        this.webhookEnabled = source["webhookEnabled"];
	        this.webhookPort = source["webhookPort"];
	        this.verifyDisabled = source["verifyDisabled"];
	        this.gitAutocommitEnabled = source["gitAutocommitEnabled"];
	        this.blurNotifyDisabled = source["blurNotifyDisabled"];
	        this.costLimitUSD = source["costLimitUSD"];
	        this.globalCostLimitUSD = source["globalCostLimitUSD"];
	        this.auxBaseURL = source["auxBaseURL"];
	        this.auxAPIKey = source["auxAPIKey"];
	        this.auxModel = source["auxModel"];
	        this.fallbackBaseURL = source["fallbackBaseURL"];
	        this.fallbackAPIKey = source["fallbackAPIKey"];
	        this.fallbackModel = source["fallbackModel"];
	        this.modelProfiles = this.convertValues(source["modelProfiles"], ModelProfile);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CozeDBField {
	    name: string;
	    type: string;
	    desc: string;
	    required: boolean;
	
	    static createFrom(source: any = {}) {
	        return new CozeDBField(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.type = source["type"];
	        this.desc = source["desc"];
	        this.required = source["required"];
	    }
	}
	export class CozeDatabase {
	    id: string;
	    name: string;
	    desc?: string;
	    fields?: CozeDBField[];
	
	    static createFrom(source: any = {}) {
	        return new CozeDatabase(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.desc = source["desc"];
	        this.fields = this.convertValues(source["fields"], CozeDBField);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CozeDatasource {
	    id: string;
	    name: string;
	    host: string;
	    port: number;
	    user: string;
	    password: string;
	    dbName: string;
	    tableName: string;
	    jdbcURL: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeDatasource(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.user = source["user"];
	        this.password = source["password"];
	        this.dbName = source["dbName"];
	        this.tableName = source["tableName"];
	        this.jdbcURL = source["jdbcURL"];
	    }
	}
	export class CozeImageGenConfig {
	    configured: boolean;
	    baseURL: string;
	    model: string;
	    apiKey: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeImageGenConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.configured = source["configured"];
	        this.baseURL = source["baseURL"];
	        this.model = source["model"];
	        this.apiKey = source["apiKey"];
	    }
	}
	export class CozeKnowledgeBase {
	    id: string;
	    name: string;
	    description?: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeKnowledgeBase(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	    }
	}
	export class CozeMCPTool {
	    name: string;
	    description: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeMCPTool(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	    }
	}
	export class CozeModelOption {
	    id: string;
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeModelOption(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	    }
	}
	export class CozeNodeExec {
	    nodeId: string;
	    nodeType: string;
	    status: string;
	    input?: string;
	    output?: string;
	    failReason?: string;
	    durationMs: number;
	
	    static createFrom(source: any = {}) {
	        return new CozeNodeExec(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.nodeId = source["nodeId"];
	        this.nodeType = source["nodeType"];
	        this.status = source["status"];
	        this.input = source["input"];
	        this.output = source["output"];
	        this.failReason = source["failReason"];
	        this.durationMs = source["durationMs"];
	    }
	}
	export class CozeRunResult {
	    status: string;
	    output: Record<string, any>;
	    outputRaw?: string;
	    durationMs: number;
	    failReason?: string;
	    execId: string;
	    nodeExecs?: CozeNodeExec[];
	
	    static createFrom(source: any = {}) {
	        return new CozeRunResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.status = source["status"];
	        this.output = source["output"];
	        this.outputRaw = source["outputRaw"];
	        this.durationMs = source["durationMs"];
	        this.failReason = source["failReason"];
	        this.execId = source["execId"];
	        this.nodeExecs = this.convertValues(source["nodeExecs"], CozeNodeExec);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CozeStatus {
	    ready: boolean;
	    modelConfigured: boolean;
	    dataDir: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ready = source["ready"];
	        this.modelConfigured = source["modelConfigured"];
	        this.dataDir = source["dataDir"];
	        this.error = source["error"];
	    }
	}
	export class CozeWorkflowDetail {
	    id: string;
	    name: string;
	    desc: string;
	    hasPublished: boolean;
	    latestVersion?: string;
	    mode: number;
	    updatedAtUnix: number;
	    canvas: string;
	    commitId: string;
	    inputJSON?: string;
	    outputJSON?: string;
	
	    static createFrom(source: any = {}) {
	        return new CozeWorkflowDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.desc = source["desc"];
	        this.hasPublished = source["hasPublished"];
	        this.latestVersion = source["latestVersion"];
	        this.mode = source["mode"];
	        this.updatedAtUnix = source["updatedAtUnix"];
	        this.canvas = source["canvas"];
	        this.commitId = source["commitId"];
	        this.inputJSON = source["inputJSON"];
	        this.outputJSON = source["outputJSON"];
	    }
	}
	export class CozeWorkflowMeta {
	    id: string;
	    name: string;
	    desc: string;
	    hasPublished: boolean;
	    latestVersion?: string;
	    mode: number;
	    updatedAtUnix: number;
	
	    static createFrom(source: any = {}) {
	        return new CozeWorkflowMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.desc = source["desc"];
	        this.hasPublished = source["hasPublished"];
	        this.latestVersion = source["latestVersion"];
	        this.mode = source["mode"];
	        this.updatedAtUnix = source["updatedAtUnix"];
	    }
	}
	export class DocCreateRequest {
	    path: string;
	    isDir: boolean;
	
	    static createFrom(source: any = {}) {
	        return new DocCreateRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.isDir = source["isDir"];
	    }
	}
	export class DocFileContent {
	    path: string;
	    name: string;
	    size: number;
	    modTime: string;
	    content: string;
	    isBinary: boolean;
	    truncated: boolean;
	
	    static createFrom(source: any = {}) {
	        return new DocFileContent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.modTime = source["modTime"];
	        this.content = source["content"];
	        this.isBinary = source["isBinary"];
	        this.truncated = source["truncated"];
	    }
	}
	export class DocInfo {
	    root: string;
	    exists: boolean;
	    fileCount: number;
	    dirCount: number;
	    totalBytes: number;
	
	    static createFrom(source: any = {}) {
	        return new DocInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.root = source["root"];
	        this.exists = source["exists"];
	        this.fileCount = source["fileCount"];
	        this.dirCount = source["dirCount"];
	        this.totalBytes = source["totalBytes"];
	    }
	}
	export class DocNode {
	    name: string;
	    displayName?: string;
	    path: string;
	    isDir: boolean;
	    size: number;
	    modTime: string;
	    children?: DocNode[];
	
	    static createFrom(source: any = {}) {
	        return new DocNode(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.displayName = source["displayName"];
	        this.path = source["path"];
	        this.isDir = source["isDir"];
	        this.size = source["size"];
	        this.modTime = source["modTime"];
	        this.children = this.convertValues(source["children"], DocNode);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DocRenameRequest {
	    from: string;
	    to: string;
	
	    static createFrom(source: any = {}) {
	        return new DocRenameRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.from = source["from"];
	        this.to = source["to"];
	    }
	}
	export class DocWriteRequest {
	    path: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new DocWriteRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.content = source["content"];
	    }
	}
	export class EmployeeView {
	    id: string;
	    name: string;
	    avatar?: string;
	    description?: string;
	    tags?: string[];
	    public: boolean;
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    empNo?: string;
	    gender?: string;
	    birthday?: string;
	    phone?: string;
	    email?: string;
	    hireDate?: string;
	    empStatus?: string;
	    deptId?: string;
	    positionId?: string;
	    roleId?: string;
	    roleIds?: string[];
	    managerId?: string;
	    workspaceRoot?: string;
	    mcpMode?: string;
	    mcpTools?: string[];
	    mcpDisabled?: boolean;
	    mcpServers?: string[];
	    skillsDisabled?: boolean;
	    skillsDeny?: string[];
	    rolePrompt?: string;
	    model?: string;
	    temperature?: number;
	    topP?: number;
	    maxTokens?: number;
	    dirs?: string[];
	    readOnly: boolean;
	    evolutionStrength?: number;
	    securitySummary: string;
	    workspacePath: string;
	    channels?: string[];
	    runnable: boolean;
	    proposals: number;
	    memoryFiles?: string[];
	    deptName?: string;
	    positionName?: string;
	    managerName?: string;
	
	    static createFrom(source: any = {}) {
	        return new EmployeeView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.avatar = source["avatar"];
	        this.description = source["description"];
	        this.tags = source["tags"];
	        this.public = source["public"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.empNo = source["empNo"];
	        this.gender = source["gender"];
	        this.birthday = source["birthday"];
	        this.phone = source["phone"];
	        this.email = source["email"];
	        this.hireDate = source["hireDate"];
	        this.empStatus = source["empStatus"];
	        this.deptId = source["deptId"];
	        this.positionId = source["positionId"];
	        this.roleId = source["roleId"];
	        this.roleIds = source["roleIds"];
	        this.managerId = source["managerId"];
	        this.workspaceRoot = source["workspaceRoot"];
	        this.mcpMode = source["mcpMode"];
	        this.mcpTools = source["mcpTools"];
	        this.mcpDisabled = source["mcpDisabled"];
	        this.mcpServers = source["mcpServers"];
	        this.skillsDisabled = source["skillsDisabled"];
	        this.skillsDeny = source["skillsDeny"];
	        this.rolePrompt = source["rolePrompt"];
	        this.model = source["model"];
	        this.temperature = source["temperature"];
	        this.topP = source["topP"];
	        this.maxTokens = source["maxTokens"];
	        this.dirs = source["dirs"];
	        this.readOnly = source["readOnly"];
	        this.evolutionStrength = source["evolutionStrength"];
	        this.securitySummary = source["securitySummary"];
	        this.workspacePath = source["workspacePath"];
	        this.channels = source["channels"];
	        this.runnable = source["runnable"];
	        this.proposals = source["proposals"];
	        this.memoryFiles = source["memoryFiles"];
	        this.deptName = source["deptName"];
	        this.positionName = source["positionName"];
	        this.managerName = source["managerName"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GhCheckInfo {
	    name: string;
	    status: string;
	    conclusion?: string;
	
	    static createFrom(source: any = {}) {
	        return new GhCheckInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.status = source["status"];
	        this.conclusion = source["conclusion"];
	    }
	}
	export class GhPRDetail {
	    number: number;
	    title: string;
	    state: string;
	    body?: string;
	    headRefName: string;
	    baseRefName: string;
	    author: string;
	    url?: string;
	    mergeable?: string;
	    reviewDecision?: string;
	    checks?: GhCheckInfo[];
	    checkState?: string;
	
	    static createFrom(source: any = {}) {
	        return new GhPRDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.number = source["number"];
	        this.title = source["title"];
	        this.state = source["state"];
	        this.body = source["body"];
	        this.headRefName = source["headRefName"];
	        this.baseRefName = source["baseRefName"];
	        this.author = source["author"];
	        this.url = source["url"];
	        this.mergeable = source["mergeable"];
	        this.reviewDecision = source["reviewDecision"];
	        this.checks = this.convertValues(source["checks"], GhCheckInfo);
	        this.checkState = source["checkState"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GhPRInfo {
	    number: number;
	    title: string;
	    state: string;
	    headRefName: string;
	    author: string;
	    isDraft: boolean;
	    checkState?: string;
	    url?: string;
	
	    static createFrom(source: any = {}) {
	        return new GhPRInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.number = source["number"];
	        this.title = source["title"];
	        this.state = source["state"];
	        this.headRefName = source["headRefName"];
	        this.author = source["author"];
	        this.isDraft = source["isDraft"];
	        this.checkState = source["checkState"];
	        this.url = source["url"];
	    }
	}
	export class GitBranchInfo {
	    name: string;
	    current: boolean;
	    isRemote: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GitBranchInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.current = source["current"];
	        this.isRemote = source["isRemote"];
	    }
	}
	export class GitCommitInfo {
	    hash: string;
	    author: string;
	    date: string;
	    subject: string;
	
	    static createFrom(source: any = {}) {
	        return new GitCommitInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hash = source["hash"];
	        this.author = source["author"];
	        this.date = source["date"];
	        this.subject = source["subject"];
	    }
	}
	export class GitFileEntry {
	    path: string;
	    status: string;
	    staged: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GitFileEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.status = source["status"];
	        this.staged = source["staged"];
	    }
	}
	export class GitStatus {
	    repo: boolean;
	    branch: string;
	    upstream: string;
	    ahead: number;
	    behind: number;
	    unstaged: GitFileEntry[];
	    staged: GitFileEntry[];
	    remoteRepo: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GitStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.repo = source["repo"];
	        this.branch = source["branch"];
	        this.upstream = source["upstream"];
	        this.ahead = source["ahead"];
	        this.behind = source["behind"];
	        this.unstaged = this.convertValues(source["unstaged"], GitFileEntry);
	        this.staged = this.convertValues(source["staged"], GitFileEntry);
	        this.remoteRepo = source["remoteRepo"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class InstallExtensionRequest {
	    repo: string;
	
	    static createFrom(source: any = {}) {
	        return new InstallExtensionRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.repo = source["repo"];
	    }
	}
	export class InstallFromMarketRequest {
	    id: string;
	
	    static createFrom(source: any = {}) {
	        return new InstallFromMarketRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	    }
	}
	export class InstallPluginFromDirRequest {
	    dir: string;
	
	    static createFrom(source: any = {}) {
	        return new InstallPluginFromDirRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.dir = source["dir"];
	    }
	}
	export class InstallPluginFromURLRequest {
	    url: string;
	
	    static createFrom(source: any = {}) {
	        return new InstallPluginFromURLRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	    }
	}
	export class MCPServer {
	    name: string;
	    transport: string;
	    command?: string;
	    args?: string[];
	    url?: string;
	    env?: Record<string, string>;
	    headers?: Record<string, string>;
	    alwaysAllow?: string[];
	    enabled: boolean;
	    disabled?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MCPServer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.transport = source["transport"];
	        this.command = source["command"];
	        this.args = source["args"];
	        this.url = source["url"];
	        this.env = source["env"];
	        this.headers = source["headers"];
	        this.alwaysAllow = source["alwaysAllow"];
	        this.enabled = source["enabled"];
	        this.disabled = source["disabled"];
	    }
	}
	export class MCPToolInfo {
	    name: string;
	    fullName: string;
	    desc?: string;
	    inputSchema?: number[];
	
	    static createFrom(source: any = {}) {
	        return new MCPToolInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.fullName = source["fullName"];
	        this.desc = source["desc"];
	        this.inputSchema = source["inputSchema"];
	    }
	}
	export class MCPServerCatalog {
	    server: string;
	    source: string;
	    tools: MCPToolInfo[];
	
	    static createFrom(source: any = {}) {
	        return new MCPServerCatalog(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.server = source["server"];
	        this.source = source["source"];
	        this.tools = this.convertValues(source["tools"], MCPToolInfo);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class MCPServerPublishRequest {
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new MCPServerPublishRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	    }
	}
	
	export class MCPToolListResult {
	    name: string;
	    ok: boolean;
	    tools?: MCPToolInfo[];
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new MCPToolListResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.ok = source["ok"];
	        this.tools = this.convertValues(source["tools"], MCPToolInfo);
	        this.error = source["error"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class MarketItem {
	    id: string;
	    kind: string;
	    name: string;
	    version?: string;
	    description?: string;
	    author?: string;
	    size?: number;
	    publishedBy?: string;
	    publishedAt?: number;
	    installed: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MarketItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.version = source["version"];
	        this.description = source["description"];
	        this.author = source["author"];
	        this.size = source["size"];
	        this.publishedBy = source["publishedBy"];
	        this.publishedAt = source["publishedAt"];
	        this.installed = source["installed"];
	    }
	}
	export class ModelItem {
	    id: string;
	    current: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.current = source["current"];
	    }
	}
	
	export class PluginView {
	    id: string;
	    name: string;
	    version?: string;
	    description?: string;
	    author?: string;
	    source?: string;
	    enabled: boolean;
	    installedAt?: number;
	    mcpInfo?: string;
	    skillCount?: number;
	
	    static createFrom(source: any = {}) {
	        return new PluginView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.version = source["version"];
	        this.description = source["description"];
	        this.author = source["author"];
	        this.source = source["source"];
	        this.enabled = source["enabled"];
	        this.installedAt = source["installedAt"];
	        this.mcpInfo = source["mcpInfo"];
	        this.skillCount = source["skillCount"];
	    }
	}
	export class ProjectEntry {
	    path: string;
	    name: string;
	    current: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ProjectEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.current = source["current"];
	    }
	}
	export class PromptDefault {
	    key: string;
	    default: string;
	
	    static createFrom(source: any = {}) {
	        return new PromptDefault(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.default = source["default"];
	    }
	}
	export class ProposalView {
	    fileName: string;
	    signature: string;
	    proposalId: string;
	    targetFile: string;
	    triggerType: string;
	    confidence: number;
	    employeeId: string;
	    content: string;
	    // Go type: time
	    createdAt: any;
	    employeeName: string;
	    targetName: string;
	
	    static createFrom(source: any = {}) {
	        return new ProposalView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileName = source["fileName"];
	        this.signature = source["signature"];
	        this.proposalId = source["proposalId"];
	        this.targetFile = source["targetFile"];
	        this.triggerType = source["triggerType"];
	        this.confidence = source["confidence"];
	        this.employeeId = source["employeeId"];
	        this.content = source["content"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.employeeName = source["employeeName"];
	        this.targetName = source["targetName"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RemoteAccountState {
	    enabled: boolean;
	    serverURL?: string;
	    loggedIn: boolean;
	    user?: string;
	    displayName?: string;
	    role?: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new RemoteAccountState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.serverURL = source["serverURL"];
	        this.loggedIn = source["loggedIn"];
	        this.user = source["user"];
	        this.displayName = source["displayName"];
	        this.role = source["role"];
	        this.error = source["error"];
	    }
	}
	export class RemoteConfig {
	    enabled: boolean;
	    serverURL: string;
	    token: string;
	    nodeID: string;
	    name: string;
	    user: string;
	    pass: string;
	
	    static createFrom(source: any = {}) {
	        return new RemoteConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.serverURL = source["serverURL"];
	        this.token = source["token"];
	        this.nodeID = source["nodeID"];
	        this.name = source["name"];
	        this.user = source["user"];
	        this.pass = source["pass"];
	    }
	}
	export class RemoteSendTaskResult {
	    taskId: string;
	    sessionId: string;
	    nodeId?: string;
	
	    static createFrom(source: any = {}) {
	        return new RemoteSendTaskResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.taskId = source["taskId"];
	        this.sessionId = source["sessionId"];
	        this.nodeId = source["nodeId"];
	    }
	}
	export class RemoteStatus {
	    enabled: boolean;
	    online: boolean;
	    error?: string;
	    nodeID?: string;
	    name?: string;
	    slot?: number;
	
	    static createFrom(source: any = {}) {
	        return new RemoteStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.online = source["online"];
	        this.error = source["error"];
	        this.nodeID = source["nodeID"];
	        this.name = source["name"];
	        this.slot = source["slot"];
	    }
	}
	export class ResourceBinding {
	    type: string;
	    id: string;
	    name: string;
	    enabled: boolean;
	    running?: boolean;
	    employeeId?: string;
	    positionId?: string;
	    assignRule?: string;
	
	    static createFrom(source: any = {}) {
	        return new ResourceBinding(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.id = source["id"];
	        this.name = source["name"];
	        this.enabled = source["enabled"];
	        this.running = source["running"];
	        this.employeeId = source["employeeId"];
	        this.positionId = source["positionId"];
	        this.assignRule = source["assignRule"];
	    }
	}
	export class RunBishengFlowRequest {
	    flowId: string;
	    vars?: Record<string, any>;
	
	    static createFrom(source: any = {}) {
	        return new RunBishengFlowRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.flowId = source["flowId"];
	        this.vars = source["vars"];
	    }
	}
	export class RunBishengFlowResult {
	    runId: string;
	    status: string;
	    reason?: string;
	    variables?: Record<string, any>;
	    history?: bisheng.Msg[];
	
	    static createFrom(source: any = {}) {
	        return new RunBishengFlowResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.runId = source["runId"];
	        this.status = source["status"];
	        this.reason = source["reason"];
	        this.variables = source["variables"];
	        this.history = this.convertValues(source["history"], bisheng.Msg);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RunFlowRequest {
	    id: string;
	    vars: Record<string, any>;
	    dryRun: boolean;
	
	    static createFrom(source: any = {}) {
	        return new RunFlowRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.vars = source["vars"];
	        this.dryRun = source["dryRun"];
	    }
	}
	export class RunNodeRequest {
	    flowId: string;
	    nodeId: string;
	    vars: Record<string, any>;
	    dryRun: boolean;
	
	    static createFrom(source: any = {}) {
	        return new RunNodeRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.flowId = source["flowId"];
	        this.nodeId = source["nodeId"];
	        this.vars = source["vars"];
	        this.dryRun = source["dryRun"];
	    }
	}
	
	export class SkillParamField {
	    name: string;
	    label: string;
	    type: string;
	    required?: boolean;
	    default?: string;
	    options?: string[];
	    description?: string;
	
	    static createFrom(source: any = {}) {
	        return new SkillParamField(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.label = source["label"];
	        this.type = source["type"];
	        this.required = source["required"];
	        this.default = source["default"];
	        this.options = source["options"];
	        this.description = source["description"];
	    }
	}
	export class SessionSkillView {
	    name: string;
	    description: string;
	    source: string;
	    modeSlugs?: string[];
	    params?: SkillParamField[];
	    values?: Record<string, string>;
	    rawValues?: string;
	
	    static createFrom(source: any = {}) {
	        return new SessionSkillView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	        this.source = source["source"];
	        this.modeSlugs = source["modeSlugs"];
	        this.params = this.convertValues(source["params"], SkillParamField);
	        this.values = source["values"];
	        this.rawValues = source["rawValues"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SkillCategoryView {
	    id: string;
	    name: string;
	    sort: number;
	    count: number;
	
	    static createFrom(source: any = {}) {
	        return new SkillCategoryView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.sort = source["sort"];
	        this.count = source["count"];
	    }
	}
	
	export class SkillView {
	    name: string;
	    description: string;
	    path: string;
	    source: string;
	    modeSlugs?: string[];
	    allowedTools?: string[];
	    disabled: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SkillView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	        this.path = source["path"];
	        this.source = source["source"];
	        this.modeSlugs = source["modeSlugs"];
	        this.allowedTools = source["allowedTools"];
	        this.disabled = source["disabled"];
	    }
	}
	export class TaskItem {
	    sessionId: string;
	    title: string;
	    workspace?: string;
	    mode?: string;
	    running: boolean;
	    isCron: boolean;
	    todoTotal: number;
	    todoDone: number;
	    // Go type: time
	    updatedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new TaskItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sessionId = source["sessionId"];
	        this.title = source["title"];
	        this.workspace = source["workspace"];
	        this.mode = source["mode"];
	        this.running = source["running"];
	        this.isCron = source["isCron"];
	        this.todoTotal = source["todoTotal"];
	        this.todoDone = source["todoDone"];
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class VersionInfo {
	    number: string;
	    buildDate: string;
	    commit: string;
	    os: string;
	    changelog: version.Entry[];
	    source: string;
	    autoCheck: boolean;
	    dismissed: string;
	    pending?: version.PendingUpgrade;
	
	    static createFrom(source: any = {}) {
	        return new VersionInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.number = source["number"];
	        this.buildDate = source["buildDate"];
	        this.commit = source["commit"];
	        this.os = source["os"];
	        this.changelog = this.convertValues(source["changelog"], version.Entry);
	        this.source = source["source"];
	        this.autoCheck = source["autoCheck"];
	        this.dismissed = source["dismissed"];
	        this.pending = this.convertValues(source["pending"], version.PendingUpgrade);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WSCreateRequest {
	    path: string;
	    isDir: boolean;
	
	    static createFrom(source: any = {}) {
	        return new WSCreateRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.isDir = source["isDir"];
	    }
	}
	export class WSFileContent {
	    path: string;
	    name: string;
	    size: number;
	    modTime: string;
	    content: string;
	    isBinary: boolean;
	    truncated?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new WSFileContent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.modTime = source["modTime"];
	        this.content = source["content"];
	        this.isBinary = source["isBinary"];
	        this.truncated = source["truncated"];
	    }
	}
	export class WSNode {
	    name: string;
	    displayName?: string;
	    path: string;
	    isDir: boolean;
	    size: number;
	    modTime: string;
	    children?: WSNode[];
	
	    static createFrom(source: any = {}) {
	        return new WSNode(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.displayName = source["displayName"];
	        this.path = source["path"];
	        this.isDir = source["isDir"];
	        this.size = source["size"];
	        this.modTime = source["modTime"];
	        this.children = this.convertValues(source["children"], WSNode);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WSRenameRequest {
	    from: string;
	    to: string;
	
	    static createFrom(source: any = {}) {
	        return new WSRenameRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.from = source["from"];
	        this.to = source["to"];
	    }
	}
	export class WSSaveRequest {
	    path: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new WSSaveRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.content = source["content"];
	    }
	}
	export class WSTreeResult {
	    root: string;
	    exists: boolean;
	    nodes: WSNode[];
	
	    static createFrom(source: any = {}) {
	        return new WSTreeResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.root = source["root"];
	        this.exists = source["exists"];
	        this.nodes = this.convertValues(source["nodes"], WSNode);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class acpTestResult {
	    name: string;
	    ok: boolean;
	    agentName?: string;
	    version?: string;
	    error?: string;
	    durationMS?: number;
	
	    static createFrom(source: any = {}) {
	        return new acpTestResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.ok = source["ok"];
	        this.agentName = source["agentName"];
	        this.version = source["version"];
	        this.error = source["error"];
	        this.durationMS = source["durationMS"];
	    }
	}
	export class cronJobView {
	    id: string;
	    name: string;
	    expr: string;
	    action?: string;
	    script?: string;
	    scriptTimeoutSec?: number;
	    scriptArgs?: string;
	    noAgent?: boolean;
	    skills?: string[];
	    flowId?: string;
	    workspace: string;
	    mode: string;
	    task: string;
	    enabled: boolean;
	    timeoutMin?: number;
	    retryLimit?: number;
	    model?: string;
	    employeeId?: string;
	    positionId?: string;
	    assignRule?: string;
	    lastAssignedIndex?: number;
	    timeWindowEnabled?: boolean;
	    startTime?: string;
	    endTime?: string;
	    failStreak?: number;
	    disabledReason?: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    // Go type: time
	    lastRun?: any;
	    // Go type: time
	    nextRun?: any;
	    // Go type: time
	    nextRetry?: any;
	    desc: string;
	
	    static createFrom(source: any = {}) {
	        return new cronJobView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.expr = source["expr"];
	        this.action = source["action"];
	        this.script = source["script"];
	        this.scriptTimeoutSec = source["scriptTimeoutSec"];
	        this.scriptArgs = source["scriptArgs"];
	        this.noAgent = source["noAgent"];
	        this.skills = source["skills"];
	        this.flowId = source["flowId"];
	        this.workspace = source["workspace"];
	        this.mode = source["mode"];
	        this.task = source["task"];
	        this.enabled = source["enabled"];
	        this.timeoutMin = source["timeoutMin"];
	        this.retryLimit = source["retryLimit"];
	        this.model = source["model"];
	        this.employeeId = source["employeeId"];
	        this.positionId = source["positionId"];
	        this.assignRule = source["assignRule"];
	        this.lastAssignedIndex = source["lastAssignedIndex"];
	        this.timeWindowEnabled = source["timeWindowEnabled"];
	        this.startTime = source["startTime"];
	        this.endTime = source["endTime"];
	        this.failStreak = source["failStreak"];
	        this.disabledReason = source["disabledReason"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.lastRun = this.convertValues(source["lastRun"], null);
	        this.nextRun = this.convertValues(source["nextRun"], null);
	        this.nextRetry = this.convertValues(source["nextRetry"], null);
	        this.desc = source["desc"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class mcpConnectResult {
	    name: string;
	    ok: boolean;
	    tools?: number;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new mcpConnectResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.ok = source["ok"];
	        this.tools = source["tools"];
	        this.error = source["error"];
	    }
	}
	export class roadmapStatsView {
	    total: number;
	    proposed: number;
	    approved: number;
	    developing: number;
	    done: number;
	    failed: number;
	    blocked: number;
	    rejected: number;
	
	    static createFrom(source: any = {}) {
	        return new roadmapStatsView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total = source["total"];
	        this.proposed = source["proposed"];
	        this.approved = source["approved"];
	        this.developing = source["developing"];
	        this.done = source["done"];
	        this.failed = source["failed"];
	        this.blocked = source["blocked"];
	        this.rejected = source["rejected"];
	    }
	}
	export class sessionSummaryLite {
	    id: string;
	    title: string;
	    messageCount: number;
	    updatedAt: number;
	    running: boolean;
	
	    static createFrom(source: any = {}) {
	        return new sessionSummaryLite(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.messageCount = source["messageCount"];
	        this.updatedAt = source["updatedAt"];
	        this.running = source["running"];
	    }
	}

}

export namespace employee {
	
	export class Department {
	    id: string;
	    code?: string;
	    name: string;
	    parentId?: string;
	    leaderId?: string;
	    sortOrder?: number;
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Department(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.code = source["code"];
	        this.name = source["name"];
	        this.parentId = source["parentId"];
	        this.leaderId = source["leaderId"];
	        this.sortOrder = source["sortOrder"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DeptTree {
	    id: string;
	    code?: string;
	    name: string;
	    parentId?: string;
	    leaderId?: string;
	    sortOrder?: number;
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    children?: DeptTree[];
	
	    static createFrom(source: any = {}) {
	        return new DeptTree(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.code = source["code"];
	        this.name = source["name"];
	        this.parentId = source["parentId"];
	        this.leaderId = source["leaderId"];
	        this.sortOrder = source["sortOrder"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.children = this.convertValues(source["children"], DeptTree);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Employee {
	    id: string;
	    name: string;
	    avatar?: string;
	    description?: string;
	    tags?: string[];
	    public: boolean;
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    empNo?: string;
	    gender?: string;
	    birthday?: string;
	    phone?: string;
	    email?: string;
	    hireDate?: string;
	    empStatus?: string;
	    deptId?: string;
	    positionId?: string;
	    roleId?: string;
	    roleIds?: string[];
	    managerId?: string;
	    workspaceRoot?: string;
	    mcpMode?: string;
	    mcpTools?: string[];
	    mcpDisabled?: boolean;
	    mcpServers?: string[];
	    skillsDisabled?: boolean;
	    skillsDeny?: string[];
	    rolePrompt?: string;
	    model?: string;
	    temperature?: number;
	    topP?: number;
	    maxTokens?: number;
	    dirs?: string[];
	    readOnly: boolean;
	    evolutionStrength?: number;
	
	    static createFrom(source: any = {}) {
	        return new Employee(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.avatar = source["avatar"];
	        this.description = source["description"];
	        this.tags = source["tags"];
	        this.public = source["public"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.empNo = source["empNo"];
	        this.gender = source["gender"];
	        this.birthday = source["birthday"];
	        this.phone = source["phone"];
	        this.email = source["email"];
	        this.hireDate = source["hireDate"];
	        this.empStatus = source["empStatus"];
	        this.deptId = source["deptId"];
	        this.positionId = source["positionId"];
	        this.roleId = source["roleId"];
	        this.roleIds = source["roleIds"];
	        this.managerId = source["managerId"];
	        this.workspaceRoot = source["workspaceRoot"];
	        this.mcpMode = source["mcpMode"];
	        this.mcpTools = source["mcpTools"];
	        this.mcpDisabled = source["mcpDisabled"];
	        this.mcpServers = source["mcpServers"];
	        this.skillsDisabled = source["skillsDisabled"];
	        this.skillsDeny = source["skillsDeny"];
	        this.rolePrompt = source["rolePrompt"];
	        this.model = source["model"];
	        this.temperature = source["temperature"];
	        this.topP = source["topP"];
	        this.maxTokens = source["maxTokens"];
	        this.dirs = source["dirs"];
	        this.readOnly = source["readOnly"];
	        this.evolutionStrength = source["evolutionStrength"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Role {
	    id: string;
	    code?: string;
	    name: string;
	    description?: string;
	    mcpMode?: string;
	    mcpTools?: string[];
	    mcpDisabled?: boolean;
	    mcpServers?: string[];
	    readOnly?: boolean;
	    skillsDisabled?: boolean;
	    skillsDeny?: string[];
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Role(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.code = source["code"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.mcpMode = source["mcpMode"];
	        this.mcpTools = source["mcpTools"];
	        this.mcpDisabled = source["mcpDisabled"];
	        this.mcpServers = source["mcpServers"];
	        this.readOnly = source["readOnly"];
	        this.skillsDisabled = source["skillsDisabled"];
	        this.skillsDeny = source["skillsDeny"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Position {
	    id: string;
	    code?: string;
	    name: string;
	    deptId?: string;
	    level?: string;
	    description?: string;
	    scopeAll?: boolean;
	    scopeDeptIds?: string[];
	    roleIds?: string[];
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Position(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.code = source["code"];
	        this.name = source["name"];
	        this.deptId = source["deptId"];
	        this.level = source["level"];
	        this.description = source["description"];
	        this.scopeAll = source["scopeAll"];
	        this.scopeDeptIds = source["scopeDeptIds"];
	        this.roleIds = source["roleIds"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class OrgView {
	    departments: Department[];
	    positions: Position[];
	    roles: Role[];
	
	    static createFrom(source: any = {}) {
	        return new OrgView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.departments = this.convertValues(source["departments"], Department);
	        this.positions = this.convertValues(source["positions"], Position);
	        this.roles = this.convertValues(source["roles"], Role);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class Proposal {
	    fileName: string;
	    signature: string;
	    proposalId: string;
	    targetFile: string;
	    triggerType: string;
	    confidence: number;
	    employeeId: string;
	    content: string;
	    // Go type: time
	    createdAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Proposal(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileName = source["fileName"];
	        this.signature = source["signature"];
	        this.proposalId = source["proposalId"];
	        this.targetFile = source["targetFile"];
	        this.triggerType = source["triggerType"];
	        this.confidence = source["confidence"];
	        this.employeeId = source["employeeId"];
	        this.content = source["content"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace evolve {
	
	export class Card {
	    id: string;
	    title: string;
	    keywords: string[];
	    body: string;
	    source?: string;
	    hits: number;
	    // Go type: time
	    lastHitAt?: any;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Card(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.keywords = source["keywords"];
	        this.body = source["body"];
	        this.source = source["source"];
	        this.hits = source["hits"];
	        this.lastHitAt = this.convertValues(source["lastHitAt"], null);
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace extension {
	
	export class Ext {
	    id: string;
	    repo: string;
	    name: string;
	    description: string;
	    author?: string;
	    version?: string;
	    skills?: string[];
	    skillDir?: string;
	    category?: string;
	    source: string;
	    installed: boolean;
	    installedAt?: number;
	    addedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new Ext(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.repo = source["repo"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.author = source["author"];
	        this.version = source["version"];
	        this.skills = source["skills"];
	        this.skillDir = source["skillDir"];
	        this.category = source["category"];
	        this.source = source["source"];
	        this.installed = source["installed"];
	        this.installedAt = source["installedAt"];
	        this.addedAt = source["addedAt"];
	    }
	}

}

export namespace flow {
	
	export class AppSummary {
	    id: string;
	    name: string;
	    desc?: string;
	    icon?: string;
	    flowCount: number;
	    createdAt: number;
	    updatedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new AppSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.desc = source["desc"];
	        this.icon = source["icon"];
	        this.flowCount = source["flowCount"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class CanvasEdge {
	    id: string;
	    source: string;
	    target: string;
	    expr?: string;
	
	    static createFrom(source: any = {}) {
	        return new CanvasEdge(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.source = source["source"];
	        this.target = source["target"];
	        this.expr = source["expr"];
	    }
	}
	export class Step {
	    type: string;
	    role?: string;
	    template?: string;
	    name?: string;
	    model?: string;
	    topK?: number;
	    mcpServers?: string[];
	    skills?: string[];
	    minScore?: number;
	
	    static createFrom(source: any = {}) {
	        return new Step(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.role = source["role"];
	        this.template = source["template"];
	        this.name = source["name"];
	        this.model = source["model"];
	        this.topK = source["topK"];
	        this.mcpServers = source["mcpServers"];
	        this.skills = source["skills"];
	        this.minScore = source["minScore"];
	    }
	}
	export class Pos {
	    x: number;
	    y: number;
	
	    static createFrom(source: any = {}) {
	        return new Pos(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.x = source["x"];
	        this.y = source["y"];
	    }
	}
	export class CanvasNode {
	    id: string;
	    type: string;
	    pos: Pos;
	    step: Step;
	
	    static createFrom(source: any = {}) {
	        return new CanvasNode(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.type = source["type"];
	        this.pos = this.convertValues(source["pos"], Pos);
	        this.step = this.convertValues(source["step"], Step);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Canvas {
	    nodes: CanvasNode[];
	    edges: CanvasEdge[];
	
	    static createFrom(source: any = {}) {
	        return new Canvas(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.nodes = this.convertValues(source["nodes"], CanvasNode);
	        this.edges = this.convertValues(source["edges"], CanvasEdge);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	export class Flow {
	    id: string;
	    name: string;
	    description?: string;
	    steps: Step[];
	    appId?: string;
	    canvas?: Canvas;
	    createdAt: number;
	    updatedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new Flow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.steps = this.convertValues(source["steps"], Step);
	        this.appId = source["appId"];
	        this.canvas = this.convertValues(source["canvas"], Canvas);
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FlowApp {
	    id: string;
	    name: string;
	    desc?: string;
	    icon?: string;
	    vars?: Record<string, string>;
	    config?: Record<string, string>;
	    appKey?: string;
	    createdAt: number;
	    updatedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new FlowApp(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.desc = source["desc"];
	        this.icon = source["icon"];
	        this.vars = source["vars"];
	        this.config = source["config"];
	        this.appKey = source["appKey"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	
	export class StepTrace {
	    index: number;
	    type: string;
	    name?: string;
	    render: string;
	    err?: string;
	    nodeId?: string;
	    docs?: number;
	    httpStatus?: number;
	
	    static createFrom(source: any = {}) {
	        return new StepTrace(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.type = source["type"];
	        this.name = source["name"];
	        this.render = source["render"];
	        this.err = source["err"];
	        this.nodeId = source["nodeId"];
	        this.docs = source["docs"];
	        this.httpStatus = source["httpStatus"];
	    }
	}
	export class RunResult {
	    flowId: string;
	    output: string;
	    steps: StepTrace[];
	    durationMs: number;
	    dryRun?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new RunResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.flowId = source["flowId"];
	        this.output = source["output"];
	        this.steps = this.convertValues(source["steps"], StepTrace);
	        this.durationMs = source["durationMs"];
	        this.dryRun = source["dryRun"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	export class Summary {
	    id: string;
	    name: string;
	    description?: string;
	    stepCount: number;
	    updatedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.stepCount = source["stepCount"];
	        this.updatedAt = source["updatedAt"];
	    }
	}

}

export namespace message {
	
	export class ClineMessage {
	    ts: number;
	    type: string;
	    ask?: string;
	    say?: string;
	    text?: string;
	    partial?: boolean;
	    images?: string[];
	
	    static createFrom(source: any = {}) {
	        return new ClineMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ts = source["ts"];
	        this.type = source["type"];
	        this.ask = source["ask"];
	        this.say = source["say"];
	        this.text = source["text"];
	        this.partial = source["partial"];
	        this.images = source["images"];
	    }
	}

}

export namespace modes {
	
	export class ModeConfig {
	    slug: string;
	    name: string;
	    roleDefinition: string;
	    whenToUse?: string;
	    description?: string;
	    groups: any[];
	    customInstructions?: string;
	    source?: string;
	
	    static createFrom(source: any = {}) {
	        return new ModeConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.slug = source["slug"];
	        this.name = source["name"];
	        this.roleDefinition = source["roleDefinition"];
	        this.whenToUse = source["whenToUse"];
	        this.description = source["description"];
	        this.groups = source["groups"];
	        this.customInstructions = source["customInstructions"];
	        this.source = source["source"];
	    }
	}

}

export namespace promptconfig {
	
	export class Config {
	    system?: string;
	    summarize?: string;
	    taskSummary?: string;
	    selfReview?: string;
	    hourlyReview?: string;
	    roadmapReview?: string;
	    selfCheckTask?: string;
	    roadmapSystem?: string;
	    autonomousDev?: string;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.system = source["system"];
	        this.summarize = source["summarize"];
	        this.taskSummary = source["taskSummary"];
	        this.selfReview = source["selfReview"];
	        this.hourlyReview = source["hourlyReview"];
	        this.roadmapReview = source["roadmapReview"];
	        this.selfCheckTask = source["selfCheckTask"];
	        this.roadmapSystem = source["roadmapSystem"];
	        this.autonomousDev = source["autonomousDev"];
	    }
	}

}

export namespace remote {
	
	export class AgentProfile {
	    id: string;
	    name: string;
	    kind?: string;
	    description?: string;
	    avatar?: string;
	    skills?: string[];
	
	    static createFrom(source: any = {}) {
	        return new AgentProfile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.description = source["description"];
	        this.avatar = source["avatar"];
	        this.skills = source["skills"];
	    }
	}
	export class Group {
	    id: string;
	    name: string;
	    owner?: string;
	    members: AgentProfile[];
	    workspace?: string;
	    nodeId?: string;
	    createdAt?: number;
	    updatedAt?: number;
	
	    static createFrom(source: any = {}) {
	        return new Group(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.owner = source["owner"];
	        this.members = this.convertValues(source["members"], AgentProfile);
	        this.workspace = source["workspace"];
	        this.nodeId = source["nodeId"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GroupMessage {
	    id?: string;
	    groupId: string;
	    author: string;
	    authorName?: string;
	    authorKind?: string;
	    text: string;
	    ts?: number;
	
	    static createFrom(source: any = {}) {
	        return new GroupMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.groupId = source["groupId"];
	        this.author = source["author"];
	        this.authorName = source["authorName"];
	        this.authorKind = source["authorKind"];
	        this.text = source["text"];
	        this.ts = source["ts"];
	    }
	}
	export class ModeInfo {
	    slug: string;
	    name: string;
	    description?: string;
	
	    static createFrom(source: any = {}) {
	        return new ModeInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.slug = source["slug"];
	        this.name = source["name"];
	        this.description = source["description"];
	    }
	}
	export class Node {
	    id: string;
	    name: string;
	    owner?: string;
	    platform?: string;
	    arch?: string;
	    version?: string;
	    hostname?: string;
	    online: boolean;
	    currentTask?: string;
	    runningTasks?: number;
	    mode?: string;
	    modes?: ModeInfo[];
	    firstSeen: number;
	    lastSeen: number;
	
	    static createFrom(source: any = {}) {
	        return new Node(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.owner = source["owner"];
	        this.platform = source["platform"];
	        this.arch = source["arch"];
	        this.version = source["version"];
	        this.hostname = source["hostname"];
	        this.online = source["online"];
	        this.currentTask = source["currentTask"];
	        this.runningTasks = source["runningTasks"];
	        this.mode = source["mode"];
	        this.modes = this.convertValues(source["modes"], ModeInfo);
	        this.firstSeen = source["firstSeen"];
	        this.lastSeen = source["lastSeen"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace roadmap {
	
	export class RunEntry {
	    outcome: string;
	    summary?: string;
	    files?: string[];
	    // Go type: time
	    startedAt?: any;
	    // Go type: time
	    finishedAt: any;
	    durationMS?: number;
	
	    static createFrom(source: any = {}) {
	        return new RunEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.outcome = source["outcome"];
	        this.summary = source["summary"];
	        this.files = source["files"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.finishedAt = this.convertValues(source["finishedAt"], null);
	        this.durationMS = source["durationMS"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Item {
	    id: string;
	    title: string;
	    detail?: string;
	    priority?: string;
	    category?: string;
	    status: string;
	    source?: string;
	    workspace?: string;
	    sessionId?: string;
	    batchId?: string;
	    parentSessionId?: string;
	    result?: string;
	    error?: string;
	    review?: string;
	    verification?: string;
	    approvedBy?: string;
	    attempts?: number;
	    blockedReason?: string;
	    runLog?: RunEntry[];
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    // Go type: time
	    startedAt?: any;
	    // Go type: time
	    finishedAt?: any;
	
	    static createFrom(source: any = {}) {
	        return new Item(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.detail = source["detail"];
	        this.priority = source["priority"];
	        this.category = source["category"];
	        this.status = source["status"];
	        this.source = source["source"];
	        this.workspace = source["workspace"];
	        this.sessionId = source["sessionId"];
	        this.batchId = source["batchId"];
	        this.parentSessionId = source["parentSessionId"];
	        this.result = source["result"];
	        this.error = source["error"];
	        this.review = source["review"];
	        this.verification = source["verification"];
	        this.approvedBy = source["approvedBy"];
	        this.attempts = source["attempts"];
	        this.blockedReason = source["blockedReason"];
	        this.runLog = this.convertValues(source["runLog"], RunEntry);
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.finishedAt = this.convertValues(source["finishedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace session {
	
	export class Summary {
	    id: string;
	    title: string;
	    parentId?: string;
	    skillCount?: number;
	    mode?: string;
	    model?: string;
	    nodeId?: string;
	    workspace?: string;
	    employeeId?: string;
	    positionId?: string;
	    assignRule?: string;
	    acpAgent?: string;
	    mcpDisabled?: boolean;
	    messageCount: number;
	    lastMessage?: string;
	    hasError?: boolean;
	    done?: boolean;
	    pinnedAt?: number;
	    hasSummary?: boolean;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    running?: boolean;
	    pendingAsk?: message.ClineMessage;
	
	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.parentId = source["parentId"];
	        this.skillCount = source["skillCount"];
	        this.mode = source["mode"];
	        this.model = source["model"];
	        this.nodeId = source["nodeId"];
	        this.workspace = source["workspace"];
	        this.employeeId = source["employeeId"];
	        this.positionId = source["positionId"];
	        this.assignRule = source["assignRule"];
	        this.acpAgent = source["acpAgent"];
	        this.mcpDisabled = source["mcpDisabled"];
	        this.messageCount = source["messageCount"];
	        this.lastMessage = source["lastMessage"];
	        this.hasError = source["hasError"];
	        this.done = source["done"];
	        this.pinnedAt = source["pinnedAt"];
	        this.hasSummary = source["hasSummary"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.running = source["running"];
	        this.pendingAsk = this.convertValues(source["pendingAsk"], message.ClineMessage);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace skills {
	
	export class Category {
	    id: string;
	    name: string;
	    sort: number;
	
	    static createFrom(source: any = {}) {
	        return new Category(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.sort = source["sort"];
	    }
	}
	export class Metadata {
	    name: string;
	    description: string;
	    path: string;
	    source: string;
	    modeSlugs?: string[];
	    allowedTools?: string[];
	
	    static createFrom(source: any = {}) {
	        return new Metadata(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	        this.path = source["path"];
	        this.source = source["source"];
	        this.modeSlugs = source["modeSlugs"];
	        this.allowedTools = source["allowedTools"];
	    }
	}

}

export namespace tools {
	
	export class SandboxConfig {
	    mode: string;
	    memoryLimitMB: number;
	    cpuPercent: number;
	    maxProcesses: number;
	
	    static createFrom(source: any = {}) {
	        return new SandboxConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.memoryLimitMB = source["memoryLimitMB"];
	        this.cpuPercent = source["cpuPercent"];
	        this.maxProcesses = source["maxProcesses"];
	    }
	}
	export class TodoItem {
	    content: string;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new TodoItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.content = source["content"];
	        this.status = source["status"];
	    }
	}

}

export namespace vector {
	
	export class Config {
	    enabled: boolean;
	    answerWith: boolean;
	    dir: string;
	    schedule: string;
	    embedURL: string;
	    embedModel: string;
	    embedKey?: string;
	    maxResults?: number;
	    minScore?: number;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.answerWith = source["answerWith"];
	        this.dir = source["dir"];
	        this.schedule = source["schedule"];
	        this.embedURL = source["embedURL"];
	        this.embedModel = source["embedModel"];
	        this.embedKey = source["embedKey"];
	        this.maxResults = source["maxResults"];
	        this.minScore = source["minScore"];
	    }
	}

}

export namespace version {
	
	export class CheckResult {
	    current: string;
	    latest: string;
	    update: boolean;
	    notes?: string[];
	    date?: string;
	    source?: string;
	    assetUrl?: string;
	    mandatory?: boolean;
	    minUpgradable?: string;
	    sha256?: string;
	
	    static createFrom(source: any = {}) {
	        return new CheckResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.current = source["current"];
	        this.latest = source["latest"];
	        this.update = source["update"];
	        this.notes = source["notes"];
	        this.date = source["date"];
	        this.source = source["source"];
	        this.assetUrl = source["assetUrl"];
	        this.mandatory = source["mandatory"];
	        this.minUpgradable = source["minUpgradable"];
	        this.sha256 = source["sha256"];
	    }
	}
	export class Entry {
	    version: string;
	    date?: string;
	    notes: string[];
	
	    static createFrom(source: any = {}) {
	        return new Entry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.date = source["date"];
	        this.notes = source["notes"];
	    }
	}
	export class PendingUpgrade {
	    target: string;
	    staged: string;
	    notes?: string;
	    startedAt: string;
	
	    static createFrom(source: any = {}) {
	        return new PendingUpgrade(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.target = source["target"];
	        this.staged = source["staged"];
	        this.notes = source["notes"];
	        this.startedAt = source["startedAt"];
	    }
	}

}

export namespace webhook {
	
	export class Hook {
	    id: string;
	    name?: string;
	    secret?: string;
	    task: string;
	    workspace?: string;
	    mode?: string;
	    enabled: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Hook(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.secret = source["secret"];
	        this.task = source["task"];
	        this.workspace = source["workspace"];
	        this.mode = source["mode"];
	        this.enabled = source["enabled"];
	    }
	}
	export class TriggerLog {
	    time: number;
	    hookId: string;
	    ip: string;
	    status: number;
	    detail?: string;
	
	    static createFrom(source: any = {}) {
	        return new TriggerLog(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time = source["time"];
	        this.hookId = source["hookId"];
	        this.ip = source["ip"];
	        this.status = source["status"];
	        this.detail = source["detail"];
	    }
	}

}

