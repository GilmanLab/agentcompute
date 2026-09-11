def main():
    sb = sandbox.create()
    catalog = image.list()
    router = instance.create(sandbox=sb["name"], name="router", image="router")
    command = instance.exec(sandbox=sb["name"], name="router", command="ip -br addr && nft list ruleset")
    lan = net.create(sandbox=sb["name"], name="lan")
    attached = net.attach(sandbox=sb["name"], instance="router", network="lan")
    observed = instance.get(sandbox=sb["name"], name="router")
    details = sandbox.get(name=sb["name"])
    extended = sandbox.extend(name=sb["name"], ttl_minutes=5)
    deleted = sandbox.delete(name=sb["name"])
    return {"sandbox": sb, "images": catalog, "instance": router, "exec": command, "lan": lan, "attached": attached, "observed": observed, "details": details, "extended": extended, "deleted": deleted}
