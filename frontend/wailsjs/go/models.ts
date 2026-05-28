export namespace main {
	
	export class ChatMessage {
	    role: string;
	    content: string;
	    timestamp: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	        this.timestamp = source["timestamp"];
	    }
	}
	export class ConnectionRecord {
	    host: string;
	    port: string;
	    token: string;
	    sessionKey: string;
	    label: string;
	    lastUsed: string;
	    usedCount: number;
	
	    static createFrom(source: any = {}) {
	        return new ConnectionRecord(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.host = source["host"];
	        this.port = source["port"];
	        this.token = source["token"];
	        this.sessionKey = source["sessionKey"];
	        this.label = source["label"];
	        this.lastUsed = source["lastUsed"];
	        this.usedCount = source["usedCount"];
	    }
	}

}

