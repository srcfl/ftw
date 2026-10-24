// FTW Cloud's terms. Statistics are part of Cloud; a box without Cloud shares
// anonymous statistics until its owner turns them off under Anonymous
// statistics (settings/tabs/fleet.js), so nobody is asked here any more.
(function (window, document) {
  "use strict";
  var read = window.fetch.bind(window);
  var details = "Recorded hourly energy, configured battery capacity, battery and charger power limits, and solar capacity help improve FTW and publish group totals. Values are rounded to 10 Wh or W. No names, addresses or device ids.";
  var privacy = "Sourceful links reports to this box for deletion. They are not anonymous and stay for 48 hours. Public values need at least 20 homes, with no home above 30% of a sum. Turning sharing off deletes unpublished reports when the box reaches the collector. Published totals remain.";
  var status, policy, authStatus, justAnswered=false, saving=false, refreshNumber=0;
  function node(tag, text, parent) {
    var el = document.createElement(tag);
    if (text) el.textContent = text;
    if (parent) parent.appendChild(el);
    return el;
  }
  function say(parent, text) { var p=node("p",text,parent);p.className="hint";return p; }
  function button(parent,text,run,primary,feedback) {
    var b=node("button",text,parent);b.type="button";b.className=primary?"btn-send":"btn-ghost";
    b.addEventListener("click",function(){
      if(saving)return;
      saving=true;
      document.querySelectorAll(".project-statistics-panel button").forEach(function(button){button.disabled=true;});
      b.disabled=true;
      b.textContent="Saving…";
      feedback.textContent="";
      feedback.hidden=true;
      Promise.resolve().then(run).then(function(){saving=false;return refresh();}).catch(function(e){
        feedback.textContent=e.message||"The choice could not be saved. Try again.";
        feedback.hidden=false;
      }).finally(function(){
        saving=false;
        document.querySelectorAll(".project-statistics-panel button").forEach(function(button){button.disabled=false;});
        b.disabled=false;b.textContent=text;
      });
    });
  }
  function write(method,path,body) {
    return window.fetch(path,{method:method,headers:{"Content-Type":"application/json"},body:body?JSON.stringify(body):undefined}).then(function(r){return r.json().then(function(v){if(!r.ok)throw Error(v.error||"The box refused the choice.");justAnswered=true;return v;});});
  }
  // What FTW Cloud's statistics add, for a panel that offers or holds Cloud.
  function cloudScope(parent) {
    say(parent,"Shares recorded hourly energy and configured system capacities. Reports stay for 48 hours; public totals require at least 20 homes.");
    var disclosure=node("details",null,parent);
    node("summary","What is shared and how it is used",disclosure);
    say(disclosure,details);
    say(disclosure,privacy);
    var link=node("a","Privacy and Cloud terms",parent);link.href="https://ftw.sourceful.energy/privacy#statistics";link.target="_blank";link.rel="noopener";
    if(!policy.available) say(parent,"You can save your choice now. Hourly reports wait for privacy review and activation.");
  }
  function paint(host,settings) {
    host.textContent="";
    // Only a Cloud box under older terms has something to answer.
    var needsTerms=!!status && status.link==="on" && !status.statisticsOnly && !status.cloudTermsAccepted;
    if (!status || !policy || (!settings && !needsTerms && !justAnswered)) {host.hidden=true;return;}
    host.hidden=false;
    if(!settings && !needsTerms && justAnswered) {
      var saved=status.link==="ending"?"Stopping Cloud and deleting its stored copy. The box continues locally.":status.cloudTermsAccepted?"Cloud terms accepted and saved on this box.":status.statisticsActive?"Statistics choice saved. Change it in Settings → FTW app.":"Statistics sharing is off. The box continues locally.";
      var receipt=say(host,saved);receipt.className="project-statistics-saved";receipt.setAttribute("role","status");
      return;
    }
    var panel=node("fieldset",null,host);
    panel.className="project-statistics-panel";
    var assistant=host.id === "access-cloud-settings";
    var cloud=status.link==="on" && !status.statisticsOnly;
    node("legend",assistant?"FTW Cloud for assistants":cloud?(status.cloudTermsAccepted?"FTW Cloud":"Review FTW Cloud terms"):"FTW Cloud",panel);
    if(assistant && !cloud) say(panel,"FTW Cloud lets your assistant reach this box. Sourceful can read its Cloud copy of your energy data. Cloud includes project statistics. Review the terms below before accepting.");
    else if(cloud) say(panel,status.cloudTermsAccepted?"The current Cloud terms are accepted. FTW Cloud includes project statistics.":"FTW Cloud includes project statistics. Sourceful can read its copy of your energy data. Accept the updated terms for the app and remote access, or turn Cloud off and keep using the box locally.");
    else say(panel,"FTW Cloud is off, and the box works locally. The anonymous statistics it shares, and the switch for them, are under Anonymous statistics below.");
    if(cloud || assistant) cloudScope(panel);
    var signIn=function(parent){
      say(parent,authStatus && authStatus.home_assistant
        ?"Home Assistant manages sign-in for this choice."
        :authStatus && authStatus.lan_auth
        ?"Your house password protects this choice. The box will ask you to log in if needed."
        :authStatus && authStatus.lan_auth===false
          ?"No house password is required. With password protection off, anyone who can reach this box on your private network can save this choice."
          :"This choice uses the same sign-in rules as Settings. A house password is optional.");
    };
    if(!status.cloudTermsAccepted && (cloud || assistant)) signIn(panel);
    var actions=node("div",null,panel);actions.className="app-link-actions";
    var feedback=node("p",null,panel);feedback.className="project-statistics-error";feedback.setAttribute("role","alert");feedback.hidden=true;
    if(status.link==="ending") {say(panel,"Cloud is stopping and deleting its stored copy. Refresh after the box receives the deletion receipt.");return;}
    if(cloud) {
      if(status.cloudTermsAccepted) say(panel,"The current Cloud terms include statistics for this box.");
      else button(actions,"Accept Cloud terms",function(){return write("POST","/api/cloud/grant",{scopes:policy.cloud_scopes,policy:policy.cloud_policy});},true,feedback);
      button(actions,"Turn FTW Cloud off",function(){
        if(!window.confirm("Turn FTW Cloud off? The app and assistants lose remote access. The box keeps working locally. You may need to pair phones again if you turn Cloud back on."))return;
        return write("DELETE","/api/cloud");
      },false,feedback);
    } else if(assistant) {
      button(actions,"Accept Cloud terms and turn it on",function(){return write("POST","/api/cloud/grant",{scopes:policy.cloud_scopes,policy:policy.cloud_policy});},true,feedback);
    } else {
      var more=node("details",null,panel);
      node("summary","Use FTW Cloud and the app",more);
      say(more,"FTW Cloud adds app and assistant remote access and a readable state copy. Statistics are part of Cloud, with no separate off switch.");
      cloudScope(more);
      signIn(more);
      button(more,"Accept Cloud terms and turn it on",function(){return write("POST","/api/cloud/grant",{scopes:policy.cloud_scopes,policy:policy.cloud_policy});},true,feedback);
    }
  }
  function refresh() {
    var current=++refreshNumber;
    return Promise.all([read("/api/cloud"),read("/api/cloud/stats-policy"),read("/api/auth/status").catch(function(){return null;})]).then(function(rs){
      return Promise.all(rs.map(function(r){return r && r.ok?r.json().catch(function(){return null;}):null;}));
    }).then(function(v){
      if(current!==refreshNumber || saving || !v[0] || !v[1])return;
      status=v[0];policy=v[1];authStatus=v[2];
      if(window.dispatchEvent && typeof CustomEvent === "function") window.dispatchEvent(new CustomEvent("ftw-cloud-choice-changed", {detail:status}));
      if(!policy.cloud_includes_statistics || !policy.standalone)return;
      var prompt=document.getElementById("project-statistics-prompt");
      if(!prompt){
        prompt=node("section");
        prompt.id="project-statistics-prompt";
        prompt.setAttribute("aria-label","Project statistics choice");
        // The Overview grid has twelve columns; the prompt needs the whole row.
        prompt.style.gridColumn="1 / -1";
        var main=document.querySelector("main")||document.body;
        main.insertBefore(prompt,main.firstChild);
      }
      paint(prompt,false);
      document.querySelectorAll("[data-project-statistics-settings]").forEach(function(host){paint(host,true);});
    }).catch(function(){});
  }
  if(!document.getElementById("project-statistics-styles")) {
    var styles=node("link");styles.id="project-statistics-styles";styles.rel="stylesheet";styles.href="/project-statistics.css";document.head.appendChild(styles);
  }
  window.FTWStatistics={refresh:refresh,cloudAccepted:function(){return !!(status && status.cloudTermsAccepted);},mount:function(host){if(status&&policy)paint(host,true);refresh();}};
  window.addEventListener("ftw-owner-ready",refresh);
  if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",refresh);else refresh();
})(window,document);
