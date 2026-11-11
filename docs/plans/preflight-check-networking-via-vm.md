Use superpowers, ponytail and subagents

Subagents:
Please make sure to follow this exactly to save
token spend
- implementer: llama.cpp Qwen3.8-27B-UD-Q4_K_M
- reviewer: openai gpt-6-luna
- overall review: openai gpt-6.1-sol

Do not select other models for use.


New feature development, plan and fix of:
https://redhat.atlassian.net/browse/SPLAT-2958 
https://redhat.atlassian.net/browse/SPLAT-2961

In preflight checking we need to create a single virtual machine
per failure domain. Use the template field to clone this virtual machine
and configure it based on the failure domain topology. Power on with the clone.
Once the machine is running and guest tools is responding GuestNicInfo and GuestStackInfo.
This should include the IPv4 IP address, default gateway and prefixLength.
Then compare the existing kubernetes node object to confirm that ip connectivity will still function
once the source machines have been recreated on the destination vcenter.
Implementation details, we should develop this in such a way that we could
eventually add extra config ignition and afterburn kargs as necessary. 
Also the ability to send in ignition files, including a kubeconfig or bash scripts.
That could be pre wired but not used in this initial iteration 

Please ask questions if you don't understand or need further clarification

